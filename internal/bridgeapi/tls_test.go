// SPDX-License-Identifier: Apache-2.0

package bridgeapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/httpx"
)

// writePair writes a self-signed pair for 127.0.0.1 with the given serial, dated
// at mod so a rotation is visible whatever the filesystem's timestamp granularity.
func writePair(t *testing.T, dir string, serial int64, mod time.Time) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "room-broker.agent-system.svc"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, DNSNames: []string{"room-broker.agent-system.svc"},
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, block := range map[string]*pem.Block{
		"tls.crt": {Type: "CERTIFICATE", Bytes: der},
		"tls.key": {Type: "EC PRIVATE KEY", Bytes: keyDER},
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// freeAddr reserves a loopback port for ListenAndServeTLS, which binds its own.
func freeAddr(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// get returns the status and the certificate the server presented, if any.
func get(t *testing.T, c *http.Client, url string) (int, *x509.Certificate, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.TLS == nil {
		return resp.StatusCode, nil, nil
	}
	return resp.StatusCode, resp.TLS.PeerCertificates[0], nil
}

// GP-18: :8443 speaks TLS only, and serves a renewed pair without a restart.
func TestPlainHTTPIsRefused(t *testing.T) {
	dir := t.TempDir()
	first := writePair(t, dir, 1, time.Now().Add(-time.Minute))
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg, err := TLSConfig(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), quiet)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Fatalf("MinVersion %x, want TLS 1.3", cfg.MinVersion)
	}
	s, _, _ := newServer(t)
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- s.ListenAndServeTLS(ctx, addr, cfg, time.Second) }()

	roots := x509.NewCertPool()
	roots.AddCert(first)
	secure := httpx.New(5*time.Second, roots)
	url := "https://" + addr + "/v1/rooms/" + room + "/events"
	var code int
	var peer *x509.Certificate
	for deadline := time.Now().Add(5 * time.Second); ; {
		if code, peer, err = get(t, secure, url); err == nil || time.Now().After(deadline) {
			break
		}
		<-time.After(20 * time.Millisecond)
	}
	if err != nil || code != http.StatusUnauthorized || peer.SerialNumber.Int64() != 1 {
		t.Fatalf("https: %d %v", code, err)
	}

	plain := httpx.New(5*time.Second, nil)
	if code, _, err := get(t, plain, "http://"+addr+"/v1/rooms/"+room+"/events"); err == nil && code != http.StatusBadRequest {
		t.Fatalf("plain http reached the API: %d", code)
	}

	// cert-manager renews the pair in place; the next handshake serves it.
	second := writePair(t, dir, 2, time.Now())
	roots.AddCert(second)
	secure = httpx.New(5*time.Second, roots)
	if code, peer, err = get(t, secure, url); err != nil || peer.SerialNumber.Int64() != 2 {
		t.Fatalf("after rotation: %d %v", code, err)
	}

	cancel()
	if err := <-served; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// A pair that fails to load mid-rotation never stops the listener: the last good one stays.
func TestABrokenRenewalKeepsTheLastGoodPair(t *testing.T) {
	dir := t.TempDir()
	writePair(t, dir, 1, time.Now().Add(-time.Minute))
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg, err := TLSConfig(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), quiet)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), []byte("half-written"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := cfg.GetCertificate(nil)
	if err != nil || c == nil || c.Leaf.SerialNumber.Int64() != 1 {
		t.Fatalf("got %v, %v", c, err)
	}
}

func TestTLSConfigRefusesAMissingPair(t *testing.T) {
	dir := t.TempDir()
	if _, err := TLSConfig(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("no pair, no error")
	}
}

// Ruling AC: every bound set. WriteTimeout is the documented exception: 0,
// because :8443 carries the SSE stream; routes bound their own writes.
func TestTheListenerSetsItsBounds(t *testing.T) {
	s, _, _ := newServer(t)
	srv := s.httpServer(":8443", nil)
	for name, v := range map[string]time.Duration{
		"ReadHeaderTimeout": srv.ReadHeaderTimeout, "ReadTimeout": srv.ReadTimeout, "IdleTimeout": srv.IdleTimeout,
	} {
		if v <= 0 {
			t.Errorf("%s unset", name)
		}
	}
	if srv.MaxHeaderBytes <= 0 || srv.WriteTimeout != 0 {
		t.Errorf("MaxHeaderBytes %d, WriteTimeout %s", srv.MaxHeaderBytes, srv.WriteTimeout)
	}
}
