// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCA(t *testing.T, dir string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "room-broker CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunBridge(t *testing.T) {
	dir := t.TempDir()
	ca := writeCA(t, dir)
	valid := map[string]string{"ROOM_ID": "3kq7x2ma", "RUN_ID": "7f3cq2xz", "CONVERSATION_ID": "c1",
		"BROKER_URL": "https://127.0.0.1:1", "BROKER_CA_FILE": ca, "ROOM_TOKEN_FILE": filepath.Join(dir, "token"),
		"HEALTH_ADDR": "127.0.0.1:0"}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for kk, vv := range valid {
			m[kk] = vv
		}
		m[k] = v
		return m
	}
	cases := []struct {
		name string
		env  map[string]string
		want string // "" means it starts, then stops cleanly with ctx
	}{
		{"it starts and stops with its context", valid, ""},
		{"a missing variable fails startup", with("RUN_ID", ""), "RUN_ID is not set"},
		{"a plain-http broker fails startup (GP-18)", with("BROKER_URL", "http://room-broker:8443"), "https"},
		{"an absent broker CA fails startup (GP-18)", with("BROKER_CA_FILE", filepath.Join(dir, "absent")), "broker CA"},
		{"a health address it cannot bind fails startup", with("HEALTH_ADDR", "256.0.0.1:1"), "health listener"},
		{"a flush grace that is not a duration fails startup", with("FLUSH_GRACE", "soon"), "FLUSH_GRACE"},
		{"a flush grace that is not positive fails startup", with("FLUSH_GRACE", "-1s"), "FLUSH_GRACE"},
		{"a flush grace past the pod's grace fails startup", with("FLUSH_GRACE", "29s"), "at most 28s"},
		{"a flush grace of 28 s starts", with("FLUSH_GRACE", "28s"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if c.want == "" {
				// The broker is unreachable: the bridge keeps saying hello until ctx ends.
				go func() { time.Sleep(100 * time.Millisecond); cancel() }()
			}
			err := RunBridge(ctx, slog.New(slog.DiscardHandler), func(k string) string { return c.env[k] })
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("RunBridge = %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("RunBridge = %v, want an error naming %q", err, c.want)
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				t.Fatal("RunBridge did not return when its context ended")
			}
		})
	}
}

func TestHealthz(t *testing.T) {
	for _, healthy := range []bool{true, false} {
		t.Run(map[bool]string{true: "healthy is 200", false: "unhealthy is 503"}[healthy], func(t *testing.T) {
			h := healthHandler(func(time.Time) bool { return healthy }, time.Now)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))
			if want := map[bool]int{true: http.StatusOK, false: http.StatusServiceUnavailable}[healthy]; rec.Code != want {
				t.Fatalf("GET /healthz = %d, want %d", rec.Code, want)
			}
		})
	}
}

// The classifier's inputs (phase 5): both optional, since CC-S5 adds BRANCH.
func TestBridgeConfigReadsTheClassifierInputs(t *testing.T) {
	env := map[string]string{"BRANCH": "agent/3kq7x2ma", "EGRESS_PROFILES": " golang, ,npm ,"}
	c, _ := loadBridgeConfig(func(k string) string { return env[k] })
	if c.branch != "agent/3kq7x2ma" || len(c.egress) != 2 || !c.egress["golang"] || !c.egress["npm"] {
		t.Fatalf("branch %q egress %v", c.branch, c.egress)
	}
	if c, _ = loadBridgeConfig(func(string) string { return "" }); c.branch != "" || len(c.egress) != 0 {
		t.Fatalf("unset: branch %q egress %v", c.branch, c.egress)
	}
}
