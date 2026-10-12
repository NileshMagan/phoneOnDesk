package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// addresses are where a phone or browser can reach this computer.
type addresses struct {
	lan, tailscale []string
}

func currentAddresses() addresses {
	var out addresses
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			ip := ipnet.IP.To4()
			if ip[0] == 100 && ip[1]&0xc0 == 64 { // 100.64.0.0/10, Tailscale's range
				out.tailscale = append(out.tailscale, ip.String())
			} else {
				out.lan = append(out.lan, ip.String())
			}
		}
	}
	// Home Wi-Fi (192.168.x) first, so the QR code points at it rather than a VPN or hotspot.
	sort.SliceStable(out.lan, func(i, j int) bool {
		return strings.HasPrefix(out.lan[i], "192.168.") && !strings.HasPrefix(out.lan[j], "192.168.")
	})
	return out
}

// localName is this computer's Bonjour name, e.g. "my-computer.local".
func localName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return ""
	}
	host = strings.ToLower(host)
	if !strings.Contains(host, ".") {
		host += ".local"
	}
	return host
}

// loadCertificate returns a self-signed certificate covering every current address. It is only
// remade when a new address appears, so a browser's one-time "continue anyway" keeps working.
func loadCertificate(dir string) (tls.Certificate, error) {
	certDir := filepath.Join(dir, "certs")
	certPath, keyPath, ipsPath := filepath.Join(certDir, "cert.pem"), filepath.Join(certDir, "key.pem"), filepath.Join(certDir, "ips.txt")
	addrs := currentAddresses()
	wanted := append(append([]string{}, addrs.lan...), addrs.tailscale...)

	var have []string
	if b, err := os.ReadFile(ipsPath); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				have = append(have, line)
			}
		}
	}
	covered := true
	for _, ip := range wanted {
		if !slices.Contains(have, ip) {
			covered = false
		}
	}
	if covered {
		if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
			return cert, nil
		}
	}

	ips := append(append([]string{}, have...), wanted...)
	ips = append(ips, "127.0.0.1")
	slices.Sort(ips)
	ips = slices.Compact(ips)
	certPEM, keyPEM, err := makeCertificate(ips, localName())
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.MkdirAll(certDir, 0o700); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(ipsPath, []byte(strings.Join(ips, "\n")+"\n"), 0o644); err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

func makeCertificate(ips []string, name string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, nil, err
	}
	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "phoneOnDesk"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	if name != "" {
		template.DNSNames = append(template.DNSNames, name)
	}
	for _, ip := range ips {
		if parsed := net.ParseIP(ip); parsed != nil {
			template.IPAddresses = append(template.IPAddresses, parsed)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

func envInt(name string, fallback int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return fallback
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func originList(list string) map[string]bool {
	out := map[string]bool{}
	for _, o := range strings.Split(list, ",") {
		if o = strings.TrimSpace(o); o != "" {
			out[o] = true
		}
	}
	return out
}
