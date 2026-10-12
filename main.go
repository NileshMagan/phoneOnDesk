// phoneOnDesk: an Android phone on your desk, seen and used from your computer's browser.
//
// The phone's browser streams its camera here (web/cam.html); the computer's browser watches it
// (web/view.html) or an allowed website such as an Ovelle page does. With the phone paired over
// adb, its screen is mirrored and controlled in the page through scrcpy's on-device server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const version = "0.4.0"

type config struct {
	httpPort, httpsPort, streamPort int
	allowedOrigins                  map[string]bool
	dataDir                         string
	appMode                         bool
}

func main() {
	appMode := flag.Bool("app", false, "open the viewer on start, and quit about 20 seconds after the last viewer closes")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("phoneOnDesk", version)
		return
	}

	// Opened by double-click (a Mac .app or a Linux AppImage), it behaves as an app.
	if exe, err := os.Executable(); err == nil && strings.Contains(exe, ".app/Contents/MacOS/") || os.Getenv("APPIMAGE") != "" {
		*appMode = true
	}

	cfg := config{
		httpPort:       envInt("CAM_HTTP_PORT", 8080),
		httpsPort:      envInt("CAM_HTTPS_PORT", 8443),
		streamPort:     envInt("CAM_STREAM_PORT", 27183),
		allowedOrigins: originList(envOr("PHONEONDESK_ORIGINS", "https://ovelle.me")),
		dataDir:        dataDir(),
		appMode:        *appMode,
	}
	if err := os.MkdirAll(cfg.dataDir, 0o700); err != nil {
		log.Fatal(err)
	}
	viewer := fmt.Sprintf("http://localhost:%d/", cfg.httpPort)

	// Opening the app twice just brings up the viewer of the one already running.
	if cfg.appMode {
		if logFile, err := os.OpenFile(filepath.Join(cfg.dataDir, "phoneondesk.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			log.SetOutput(logFile)
		}
		if alreadyRunning(cfg.httpPort) {
			openBrowser(viewer)
			return
		}
	}

	srv, err := newServer(cfg)
	if err != nil {
		log.Fatal(err)
	}
	errs := make(chan error, 2)
	go func() { errs <- srv.http.ListenAndServe() }()
	go func() { errs <- srv.https.ListenAndServeTLS("", "") }()
	log.Printf("phoneOnDesk %s · viewer %s · phone https://<this computer>:%d/cam", version, viewer, cfg.httpsPort)
	if cfg.appMode {
		time.AfterFunc(300*time.Millisecond, func() { openBrowser(viewer) })
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			srv.shutdown("could not start: " + err.Error())
			log.Fatal(err)
		}
	case <-stop:
		srv.shutdown("stopped")
	case reason := <-srv.quit:
		srv.shutdown(reason)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.http.Shutdown(ctx)
	_ = srv.https.Shutdown(ctx)
}

func dataDir() string {
	if dir := os.Getenv("PHONEONDESK_DATA_DIR"); dir != "" {
		return dir
	}
	base, err := os.UserConfigDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "phoneOnDesk")
}

func alreadyRunning(port int) bool {
	client := http.Client{Timeout: 700 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/status", port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func openBrowser(url string) {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	if err := exec.Command(opener, url).Start(); err != nil {
		log.Printf("open %s yourself: %v", url, err)
	}
}
