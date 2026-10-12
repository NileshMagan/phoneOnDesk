package main

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// adbTool finds adb (Android's debugging tool, which pairs with the phone and carries its
// screen), or downloads Google's own copy into the data folder when the reader asks for it.
type adbTool struct {
	dataDir string
	mu      sync.Mutex
	path    string

	devices *shortCache[[]device]
	battery *shortCache[batteryInfo]
}

type device struct {
	Serial string `json:"serial"`
	State  string `json:"state"`
	Model  string `json:"model"`
}

type batteryInfo struct {
	Level    *int `json:"level"`
	Charging bool `json:"charging"`
}

func newAdb(dataDir string) *adbTool {
	a := &adbTool{dataDir: dataDir}
	a.devices = newShortCache(3*time.Second, a.listDevices)
	a.battery = newShortCache(20*time.Second, a.readBattery)
	return a
}

func (a *adbTool) ownCopy() string {
	return filepath.Join(a.dataDir, "platform-tools", "adb")
}

// find returns adb's path, or "" when there is none yet.
func (a *adbTool) find() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.path != "" {
		if _, err := os.Stat(a.path); err == nil {
			return a.path
		}
	}
	candidates := []string{a.ownCopy()}
	if p, err := exec.LookPath("adb"); err == nil {
		candidates = append(candidates, p)
	}
	// Apps opened from Finder don't get Homebrew on PATH.
	candidates = append(candidates, "/opt/homebrew/bin/adb", "/usr/local/bin/adb", "/usr/bin/adb")
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			a.path = c
			return c
		}
	}
	return ""
}

func (a *adbTool) run(timeout time.Duration, args ...string) (string, error) {
	path := a.find()
	if path == "" {
		return "", errors.New("Android tools aren't installed yet")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

var modelPattern = regexp.MustCompile(`model:(\S+)`)

func (a *adbTool) listDevices() ([]device, error) {
	out, err := a.run(15*time.Second, "devices", "-l")
	if err != nil && out == "" {
		return nil, err
	}
	var list []device
	for _, line := range strings.Split(out, "\n")[1:] {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "*") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		d := device{Serial: fields[0], State: fields[1], Model: fields[0]}
		if m := modelPattern.FindStringSubmatch(line); m != nil {
			d.Model = strings.ReplaceAll(m[1], "_", " ")
		}
		list = append(list, d)
	}
	return list, nil
}

// readyDevice is the first phone that is paired and allowed this computer.
func (a *adbTool) readyDevice() (device, bool) {
	list, _ := a.devices.get()
	for _, d := range list {
		if d.State == "device" {
			return d, true
		}
	}
	return device{}, false
}

var (
	levelPattern   = regexp.MustCompile(`\blevel: (\d+)`)
	poweredPattern = regexp.MustCompile(`(AC|USB|Wireless) powered: true`)
)

func (a *adbTool) readBattery() (batteryInfo, error) {
	out, err := a.run(10*time.Second, "shell", "dumpsys", "battery")
	if err != nil {
		return batteryInfo{}, err
	}
	var info batteryInfo
	if m := levelPattern.FindStringSubmatch(out); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			info.Level = &n
		}
	}
	info.Charging = poweredPattern.MatchString(out)
	return info, nil
}

// platformToolsURL is Google's official download of adb for this computer.
func platformToolsURL() string {
	if u := os.Getenv("PHONEONDESK_PLATFORM_TOOLS_URL"); u != "" {
		return u
	}
	osName := "linux"
	if runtime.GOOS == "darwin" {
		osName = "darwin"
	}
	return fmt.Sprintf("https://dl.google.com/android/repository/platform-tools-latest-%s.zip", osName)
}

// install downloads Google's platform-tools and keeps them in the data folder.
func (a *adbTool) install() error {
	client := http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(platformToolsURL())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download answered %s", resp.Status)
	}
	tmp, err := os.CreateTemp("", "platform-tools-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	size, err := io.Copy(tmp, io.LimitReader(resp.Body, 200<<20))
	tmp.Close()
	if err != nil {
		return err
	}
	reader, err := zip.OpenReader(tmp.Name())
	if err != nil {
		return fmt.Errorf("the download (%d bytes) isn't a zip: %w", size, err)
	}
	defer reader.Close()

	staging := filepath.Join(a.dataDir, "platform-tools.partial")
	os.RemoveAll(staging)
	for _, f := range reader.File {
		name := filepath.Clean(f.Name)
		if !strings.HasPrefix(name, "platform-tools") || strings.Contains(name, "..") {
			continue
		}
		target := filepath.Join(staging, strings.TrimPrefix(name, "platform-tools"))
		if f.FileInfo().IsDir() {
			os.MkdirAll(target, 0o755)
			continue
		}
		if err := extract(f, target); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(staging, "adb")); err != nil {
		return errors.New("the download had no adb in it")
	}
	final := filepath.Join(a.dataDir, "platform-tools")
	os.RemoveAll(final)
	if err := os.Rename(staging, final); err != nil {
		return err
	}
	a.mu.Lock()
	a.path = ""
	a.mu.Unlock()
	return nil
}

func extract(f *zip.File, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	src, err := f.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	mode := f.Mode().Perm() | 0o600
	dst, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, io.LimitReader(src, 100<<20))
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	return err
}

// shortCache shares one slow lookup between the pages polling for it.
type shortCache[T any] struct {
	ttl  time.Duration
	load func() (T, error)
	mu   sync.Mutex
	at   time.Time
	val  T
	err  error
}

func newShortCache[T any](ttl time.Duration, load func() (T, error)) *shortCache[T] {
	return &shortCache[T]{ttl: ttl, load: load}
}

func (c *shortCache[T]) get() (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < c.ttl {
		return c.val, c.err
	}
	c.val, c.err = c.load()
	c.at = time.Now()
	return c.val, c.err
}

func (c *shortCache[T]) forget() {
	c.mu.Lock()
	c.at = time.Time{}
	c.mu.Unlock()
}
