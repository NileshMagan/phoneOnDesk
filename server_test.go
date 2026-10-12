package main

import (
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testServer(t *testing.T) *server {
	t.Helper()
	s, err := newServer(config{httpPort: 8080, httpsPort: 8443, streamPort: 27183,
		allowedOrigins: originList("https://ovelle.me"), dataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func request(method, target string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = "127.0.0.1:50000"
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestWhoMayAsk(t *testing.T) {
	s := testServer(t)
	cases := []struct {
		name    string
		method  string
		target  string
		headers map[string]string
		want    int
	}{
		{"our own viewer", "GET", "http://localhost:8080/status", nil, 200},
		{"DNS rebinding: a website's name pointed here", "GET", "http://evil.example:8080/clip", nil, 421},
		{"another site's script", "POST", "http://localhost:8080/phone/control", map[string]string{"Origin": "https://evil.example"}, 403},
		{"another site's <img> with no Origin", "GET", "http://localhost:8080/phone/battery", map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "no-cors"}, 403},
		{"a link from another site to a page", "GET", "http://localhost:8080/connected", map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate"}, 200},
		{"a link from another site to an action", "GET", "http://localhost:8080/clip", map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate"}, 403},
		{"the allowed site's preflight", "OPTIONS", "http://localhost:8080/phone/control", map[string]string{"Origin": "https://ovelle.me", "Access-Control-Request-Method": "POST"}, 204},
		{"null origin", "GET", "http://localhost:8080/status", map[string]string{"Origin": "null"}, 403},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		s.handle(w, request(c.method, c.target, c.headers))
		if w.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, w.Code, c.want)
		}
	}

	w := httptest.NewRecorder()
	s.handle(w, request("OPTIONS", "http://localhost:8080/phone/control", map[string]string{"Origin": "https://ovelle.me"}))
	if w.Header().Get("Access-Control-Allow-Origin") != "https://ovelle.me" || w.Header().Get("Access-Control-Allow-Private-Network") != "true" {
		t.Errorf("allowed site is missing CORS or private-network headers: %v", w.Header())
	}
}

func TestPhoneRoutesAreForThisComputerOnly(t *testing.T) {
	s := testServer(t)
	for _, path := range []string{"/hello", "/adb", "/phone/battery", "/phone/stream"} {
		r := request("GET", "http://localhost:8080"+path, nil)
		r.RemoteAddr = "192.168.1.50:40000"
		w := httptest.NewRecorder()
		s.handle(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s from the network: got %d, want 403", path, w.Code)
		}
	}
}

func TestCertificateCoversLocalhost(t *testing.T) {
	dir := t.TempDir()
	cert, err := loadCertificate(dir)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.VerifyHostname("localhost"); err != nil {
		t.Errorf("certificate doesn't cover localhost: %v", err)
	}
	if err := leaf.VerifyHostname("127.0.0.1"); err != nil {
		t.Errorf("certificate doesn't cover 127.0.0.1: %v", err)
	}
	// Loading again reuses it, so a browser's one-time acceptance survives restarts.
	again, err := loadCertificate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(again.Certificate[0]) != string(cert.Certificate[0]) {
		t.Error("certificate was remade although no new address appeared")
	}
}

func TestFrameMessage(t *testing.T) {
	got := frameMessage(msgKey, []byte{9, 8})
	want := []byte{msgKey, 0, 0, 0, 2, 9, 8}
	if string(got) != string(want) {
		t.Errorf("frameMessage = %v, want %v", got, want)
	}
}

func TestOthersOnTheWiFiNeedThePhoneKey(t *testing.T) {
	s := testServer(t)
	fromWiFi := func(target string, cookie string) *httptest.ResponseRecorder {
		r := request("GET", target, nil)
		r.RemoteAddr = "192.168.1.77:40000"
		r.Host = "127.0.0.1:8443"
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: keyCookie, Value: cookie})
		}
		w := httptest.NewRecorder()
		s.handle(w, r)
		return w
	}
	for _, path := range []string{"/clip", "/snapshot.jpg", "/status", "/events", "/cam"} {
		if w := fromWiFi("https://127.0.0.1:8443"+path, ""); w.Code != http.StatusForbidden {
			t.Errorf("%s without the key: got %d, want 403", path, w.Code)
		}
	}
	if w := fromWiFi("https://127.0.0.1:8443/clip", "wrong"); w.Code != http.StatusForbidden {
		t.Errorf("wrong key: got %d, want 403", w.Code)
	}
	w := fromWiFi("https://127.0.0.1:8443/cam?key="+s.phoneKey, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Set-Cookie"), keyCookie+"="+s.phoneKey) {
		t.Errorf("scanning the QR code: got %d, cookie %q", w.Code, w.Header().Get("Set-Cookie"))
	}
	if w := fromWiFi("https://127.0.0.1:8443/clip", s.phoneKey); w.Code != http.StatusOK {
		t.Errorf("phone holding the key: got %d, want 200", w.Code)
	}
	// The key is never handed out over the network.
	if w := fromWiFi("https://127.0.0.1:8443/info", s.phoneKey); strings.Contains(w.Body.String(), s.phoneKey) {
		t.Error("/info gave the key to a device on the network")
	}
}
