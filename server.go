package main

import (
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFiles embed.FS

const quitAfter = 20 * time.Second

type cameraStatus struct {
	Level    *int  `json:"level"`
	Charging *bool `json:"charging"`
	At       int64 `json:"at"`
	W        int   `json:"w"`
	H        int   `json:"h"`
}

type clipSlot struct {
	Text string `json:"text"`
	At   int64  `json:"at"`
}

type server struct {
	cfg   config
	adb   *adbTool
	http  *http.Server
	https *http.Server
	quit  chan string

	mu       sync.Mutex
	frame    []byte
	status   cameraStatus
	watchers map[chan int64]struct{}
	quitTmr  *time.Timer
	clip     struct{ ToMac, ToPhone clipSlot }
	streams  map[string]*phoneStream

	hostsMu sync.Mutex
	hosts   map[string]bool
	hostsAt time.Time

	// phoneKey is the secret in the QR code. Anything not on this computer must hold it, so
	// someone else on the same Wi-Fi can't watch the camera or read the clipboard.
	phoneKey string
}

func newServer(cfg config) (*server, error) {
	cert, err := loadCertificate(cfg.dataDir)
	if err != nil {
		return nil, fmt.Errorf("making this computer's certificate: %w", err)
	}
	key, err := loadPhoneKey(cfg.dataDir)
	if err != nil {
		return nil, fmt.Errorf("making the phone's key: %w", err)
	}
	s := &server{cfg: cfg, adb: newAdb(cfg.dataDir), quit: make(chan string, 1), watchers: map[chan int64]struct{}{}, streams: map[string]*phoneStream{}, phoneKey: key}
	handler := http.HandlerFunc(s.handle)
	s.http = &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	s.https = &http.Server{Addr: fmt.Sprintf(":%d", cfg.httpsPort), Handler: handler, ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	// In app mode, give the browser a minute to open the viewer before deciding nobody's watching.
	if cfg.appMode {
		s.quitTmr = time.AfterFunc(time.Minute, func() { s.quitIfUnwatched("the viewer never opened") })
	}
	return s, nil
}

func (s *server) shutdown(reason string) {
	log.Printf("stopping: %s", reason)
	s.mu.Lock()
	streams := make([]*phoneStream, 0, len(s.streams))
	for _, st := range s.streams {
		streams = append(streams, st)
	}
	s.mu.Unlock()
	for _, st := range streams {
		st.stop("phoneOnDesk is stopping")
	}
}

func (s *server) quitIfUnwatched(reason string) {
	s.mu.Lock()
	idle := len(s.watchers) == 0
	s.mu.Unlock()
	if idle {
		select {
		case s.quit <- reason:
		default:
		}
	}
}

// ---------------------------------------------------------------- who may ask

// knownHost reports whether the request is addressed to one of this computer's own names. That
// blocks DNS rebinding: a website that points its own name here would otherwise look same-origin.
func (s *server) knownHost(r *http.Request) bool {
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	s.hostsMu.Lock()
	defer s.hostsMu.Unlock()
	if s.hosts == nil || time.Since(s.hostsAt) > 10*time.Second {
		addrs := currentAddresses()
		s.hosts = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
		if name := localName(); name != "" {
			s.hosts[name] = true
		}
		for _, ip := range append(addrs.lan, addrs.tailscale...) {
			s.hosts[ip] = true
		}
		s.hostsAt = time.Now()
	}
	return s.hosts[host]
}

// Pages another website may simply link to; nothing else answers it without an allowed Origin.
var linkablePages = map[string]bool{"/": true, "/view": true, "/cam": true, "/connected": true}

// checkOrigin lets our own pages in, and other websites only from the allow list. A request a site
// sends without an Origin (an <img>, a link) may only open a page, so an unrelated site you visit
// can neither read anything nor tap, pair, wake or stop the phone.
func (s *server) checkOrigin(w http.ResponseWriter, r *http.Request) bool {
	if !s.knownHost(r) {
		http.Error(w, "unknown host", http.StatusMisdirectedRequest)
		return false
	}
	origin := r.Header.Get("Origin")
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	self := scheme + "://" + r.Host
	allowed := origin != "" && s.cfg.allowedOrigins[origin]
	site := r.Header.Get("Sec-Fetch-Site")
	navigates := r.Method == http.MethodGet && r.Header.Get("Sec-Fetch-Mode") == "navigate" && linkablePages[r.URL.Path]
	if (origin != "" && origin != self && !allowed) || (!allowed && (site == "cross-site" || site == "same-site") && !navigates) {
		http.Error(w, "not allowed from this website", http.StatusForbidden)
		return false
	}
	if !allowed {
		return true
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Add("Vary", "Origin")
	if r.Method == http.MethodOptions {
		h.Set("Access-Control-Allow-Methods", "GET, POST")
		if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
			h.Set("Access-Control-Allow-Headers", req)
		} else {
			h.Set("Access-Control-Allow-Headers", "content-type")
		}
		// Chrome's check before a public website may reach a program on this computer.
		h.Set("Access-Control-Allow-Private-Network", "true")
		h.Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return false
	}
	return true
}

func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---------------------------------------------------------------- routes

func (s *server) handle(w http.ResponseWriter, r *http.Request) {
	if !s.checkOrigin(w, r) {
		return
	}
	path := r.URL.Path

	if !isLoopback(r) && !s.phoneAllowed(w, r) {
		return
	}

	// Routes that act on the phone or this computer answer only the computer itself.
	if path == "/hello" || path == "/quit" || path == "/adb" || strings.HasPrefix(path, "/adb/") || strings.HasPrefix(path, "/phone/") {
		if !isLoopback(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "Only this computer can do that."})
			return
		}
	}

	switch {
	case path == "/frame" && r.Method == http.MethodPost:
		s.postFrame(w, r)
	case path == "/events":
		s.events(w, r)
	case path == "/snapshot.jpg":
		s.mu.Lock()
		frame := s.frame
		s.mu.Unlock()
		if frame == nil {
			http.Error(w, "no frame yet", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(frame)
	case path == "/status":
		s.mu.Lock()
		st := s.status
		s.mu.Unlock()
		var age *int64
		if st.At > 0 {
			a := time.Now().UnixMilli() - st.At
			age = &a
		}
		writeJSON(w, http.StatusOK, map[string]any{"level": st.Level, "charging": st.Charging, "at": st.At, "w": st.W, "h": st.H, "ageMs": age})
	case path == "/info":
		addrs := currentAddresses()
		// The key goes only to this computer's own viewer, which shows it as the QR code.
		key := ""
		if isLoopback(r) {
			key = s.phoneKey
		}
		urls := map[string]any{"named": nil, "lan": phoneURLs(addrs.lan, s.cfg.httpsPort, key), "tailscale": phoneURLs(addrs.tailscale, s.cfg.httpsPort, key)}
		if name := localName(); name != "" {
			urls["named"] = camURL(name, s.cfg.httpsPort, key)
		}
		writeJSON(w, http.StatusOK, map[string]any{"phoneUrls": urls, "appMode": s.cfg.appMode, "version": version})
	case path == "/hello":
		hello := map[string]any{"app": "phoneOnDesk", "version": version, "phone": nil, "camera": map[string]int64{"at": s.cameraAt()}}
		if d, ok := s.adb.readyDevice(); ok {
			hello["phone"] = map[string]string{"model": d.Model}
		}
		writeJSON(w, http.StatusOK, hello)
	case path == "/connected":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, connectedPage)
	case path == "/clip":
		s.clipRoute(w, r)
	case path == "/quit" && r.Method == http.MethodPost:
		w.WriteHeader(http.StatusNoContent)
		go func() { time.Sleep(100 * time.Millisecond); s.quit <- "the Stop button" }()
	case path == "/adb" && r.Method == http.MethodGet:
		s.adbState(w)
	case path == "/adb/install" && r.Method == http.MethodPost:
		if err := s.adb.install(); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "out": "Couldn't download Android's tools: " + err.Error()})
			return
		}
		s.adb.devices.forget()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "out": "Android's tools are installed."})
	case (path == "/adb/pair" || path == "/adb/connect") && r.Method == http.MethodPost:
		s.adbPairOrConnect(w, r, path == "/adb/pair")
	case path == "/phone/stream":
		s.phoneStreamRoute(w, r)
	case path == "/phone/control" && r.Method == http.MethodPost:
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		s.mu.Lock()
		st := s.streams["display"]
		s.mu.Unlock()
		if st != nil && len(body) > 0 {
			st.sendControl(body)
		}
		w.WriteHeader(http.StatusNoContent)
	case path == "/phone/battery":
		b, err := s.adb.battery.get()
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{"level": nil, "charging": nil})
			return
		}
		writeJSON(w, http.StatusOK, b)
	default:
		s.page(w, r)
	}
}

var pages = map[string]string{"/": "web/view.html", "/view": "web/view.html", "/cam": "web/cam.html", "/phone-video.js": "web/phone-video.js"}

func (s *server) page(w http.ResponseWriter, r *http.Request) {
	name, ok := pages[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	body, err := webFiles.ReadFile(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	kind := "text/html; charset=utf-8"
	if strings.HasSuffix(name, ".js") {
		kind = "text/javascript; charset=utf-8"
	}
	w.Header().Set("Content-Type", kind)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(body)
}

func phoneURLs(ips []string, port int, key string) []string {
	out := []string{}
	for _, ip := range ips {
		out = append(out, camURL(ip, port, key))
	}
	return out
}

func camURL(host string, port int, key string) string {
	u := fmt.Sprintf("https://%s:%d/cam", host, port)
	if key != "" {
		u += "?key=" + key
	}
	return u
}

const keyCookie = "phoneondesk_key"

// phoneAllowed lets in a device holding the phone key: from its cookie, or from the QR code's
// link, which then sets the cookie so the page's own requests carry it.
func (s *server) phoneAllowed(w http.ResponseWriter, r *http.Request) bool {
	matches := func(v string) bool { return subtle.ConstantTimeCompare([]byte(v), []byte(s.phoneKey)) == 1 }
	if c, err := r.Cookie(keyCookie); err == nil && matches(c.Value) {
		return true
	}
	if r.URL.Path == "/cam" && matches(r.URL.Query().Get("key")) {
		http.SetCookie(w, &http.Cookie{Name: keyCookie, Value: s.phoneKey, Path: "/", MaxAge: 400 * 24 * 3600,
			Secure: r.TLS != nil, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		return true
	}
	if r.URL.Path == "/cam" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, scanAgainPage)
		return false
	}
	http.Error(w, "scan the QR code on your computer first", http.StatusForbidden)
	return false
}

// loadPhoneKey keeps one key, so a phone that scanned once (or bookmarked the page) keeps working.
func loadPhoneKey(dir string) (string, error) {
	path := filepath.Join(dir, "phone-key")
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
		return strings.TrimSpace(string(b)), nil
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	key := hex.EncodeToString(raw)
	return key, os.WriteFile(path, []byte(key+"\n"), 0o600)
}

func (s *server) cameraAt() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status.At
}

// ---------------------------------------------------------------- camera page frames

func (s *server) postFrame(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil || len(body) == 0 {
		http.Error(w, "no picture", http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	st := cameraStatus{At: time.Now().UnixMilli()}
	st.W, _ = strconv.Atoi(q.Get("w"))
	st.H, _ = strconv.Atoi(q.Get("h"))
	if level, err := strconv.Atoi(q.Get("battery")); err == nil {
		st.Level = &level
		charging := q.Get("charging") == "1"
		st.Charging = &charging
	}
	s.mu.Lock()
	s.frame = body
	s.status = st
	for ch := range s.watchers {
		select {
		case ch <- st.At:
		default: // that viewer still has a ping waiting; it fetches the newest picture anyway
		}
	}
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// events sends one server-sent event per new frame. Open connections count as viewers.
func (s *server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	ch := make(chan int64, 1)
	s.mu.Lock()
	s.watchers[ch] = struct{}{}
	if s.quitTmr != nil {
		s.quitTmr.Stop()
	}
	at := s.status.At
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.watchers, ch)
		if len(s.watchers) == 0 && s.cfg.appMode {
			s.quitTmr = time.AfterFunc(quitAfter, func() { s.quitIfUnwatched("the viewer closed") })
		}
		s.mu.Unlock()
	}()

	io.WriteString(w, "retry: 2000\n\n")
	if at > 0 {
		fmt.Fprintf(w, "data: %d\n\n", at)
	}
	flusher.Flush()
	keepAlive := time.NewTicker(15 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case at := <-ch:
			fmt.Fprintf(w, "data: %d\n\n", at)
		case <-keepAlive.C:
			io.WriteString(w, ": ping\n\n")
		case <-r.Context().Done():
			return
		}
		flusher.Flush()
	}
}

// ---------------------------------------------------------------- clipboard

func (s *server) clipRoute(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		defer s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]clipSlot{"toMac": s.clip.ToMac, "toPhone": s.clip.ToPhone})
	case http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
		if err != nil || len(body) > 1<<20 {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "Too much text"})
			return
		}
		slot := clipSlot{Text: string(body), At: time.Now().UnixMilli()}
		s.mu.Lock()
		if r.URL.Query().Get("to") == "phone" {
			s.clip.ToPhone = slot
		} else {
			s.clip.ToMac = slot
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// ---------------------------------------------------------------- phone over adb

func (s *server) adbState(w http.ResponseWriter) {
	if s.adb.find() == "" {
		writeJSON(w, http.StatusOK, map[string]any{"tools": false, "devices": []device{}})
		return
	}
	list, _ := s.adb.devices.get()
	if list == nil {
		list = []device{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": true, "devices": list})
}

var targetPattern = regexp.MustCompile(`^[\w.-]+:\d+$`)

func (s *server) adbPairOrConnect(w http.ResponseWriter, r *http.Request, pair bool) {
	var body struct {
		Target string `json:"target"`
		Code   string `json:"code"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
	if !targetPattern.MatchString(body.Target) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "out": "Use the IP address:port shown on the phone, e.g. 192.168.1.20:37123"})
		return
	}
	args := []string{"connect", body.Target}
	if pair {
		code := regexp.MustCompile(`\D`).ReplaceAllString(body.Code, "")
		args = []string{"pair", body.Target, code}
	}
	out, err := s.adb.run(20*time.Second, args...)
	s.adb.devices.forget()
	ok := err == nil && !regexp.MustCompile(`(?i)fail|unable|error`).MatchString(out)
	if out == "" {
		out = "Done"
		if !ok {
			out = "Couldn't reach the phone. Check the address and that Wireless debugging is on."
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "out": out})
}

func (s *server) getStream(source string) (*phoneStream, error) {
	s.mu.Lock()
	if st := s.streams[source]; st != nil && !st.isStopped() {
		s.mu.Unlock()
		return st, st.wait()
	}
	s.mu.Unlock()
	if s.adb.find() == "" {
		return nil, fmt.Errorf("Android tools aren't installed yet")
	}
	d, ok := s.adb.readyDevice()
	if !ok {
		return nil, fmt.Errorf("No phone connected. Pair it first.")
	}
	port := s.cfg.streamPort
	if source == "camera" {
		port++
	}
	st := newPhoneStream(s.adb, d.Serial, source, port)
	st.onEnd = func(string) {
		s.mu.Lock()
		if s.streams[source] == st {
			delete(s.streams, source)
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	if existing := s.streams[source]; existing != nil && !existing.isStopped() {
		s.mu.Unlock()
		return existing, existing.wait()
	}
	s.streams[source] = st
	s.mu.Unlock()
	go st.start()
	return st, st.wait()
}

func (s *server) phoneStreamRoute(w http.ResponseWriter, r *http.Request) {
	source := "display"
	if r.URL.Query().Get("source") == "camera" {
		source = "camera"
	}
	st, err := s.getStream(source)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	client, remove := st.addClient()
	defer remove()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	for {
		select {
		case message, open := <-client.out:
			if !open {
				return
			}
			if _, err := w.Write(message); err != nil {
				return
			}
			// Send everything already queued in one flush.
			for drained := false; !drained; {
				select {
				case more, open := <-client.out:
					if !open {
						flusher.Flush()
						return
					}
					w.Write(more)
				default:
					drained = true
				}
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

const scanAgainPage = `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>phoneOnDesk</title><body style="font:16px system-ui;background:#0b0b0c;color:#eee;display:grid;place-items:center;height:100vh;margin:0;text-align:center;padding:0 24px">
<div><h1 style="font-size:20px">Scan the code on your computer</h1><p style="opacity:.7">Open phoneOnDesk on your computer and scan the QR code it shows. It carries a key, so only your phone can send the camera there.</p></div>`

const connectedPage = `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>phoneOnDesk connected</title><body style="font:16px system-ui;background:#0b0b0c;color:#eee;display:grid;place-items:center;height:100vh;margin:0;text-align:center">
<div><div style="font-size:40px">✓</div><h1 style="font-size:20px">This browser can now reach phoneOnDesk</h1><p style="opacity:.7">Close this tab and go back to your page.</p></div>`
