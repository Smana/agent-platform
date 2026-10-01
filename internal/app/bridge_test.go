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
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/bridge"
	"github.com/Smana/agent-platform/internal/wire"
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
			h := healthHandler(func(time.Time) bool { return healthy }, func() bridge.Admission { return bridge.Admission{} }, time.Now)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))
			if want := map[bool]int{true: http.StatusOK, false: http.StatusServiceUnavailable}[healthy]; rec.Code != want {
				t.Fatalf("GET /healthz = %d, want %d", rec.Code, want)
			}
		})
	}
}

// F15: /admission is what room-bridge gate reads before the harness may start.
func TestAdmissionEndpoint(t *testing.T) {
	cases := []struct {
		name    string
		a       bridge.Admission
		code    int
		bodyHas string
	}{
		{"pending is 503", bridge.Admission{}, http.StatusServiceUnavailable, "pending"},
		{"admitted is 200", bridge.Admission{Admitted: true}, http.StatusOK, "admitted"},
		{"refused is 409 with the reason", bridge.Admission{Refused: wire.ReasonRoomBusy}, http.StatusConflict, wire.ReasonRoomBusy},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := healthHandler(func(time.Time) bool { return true }, func() bridge.Admission { return c.a }, time.Now)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admission", nil))
			if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.bodyHas) {
				t.Fatalf("GET /admission = %d %q, want %d naming %q", rec.Code, rec.Body.String(), c.code, c.bodyHas)
			}
		})
	}
}

// F15: the gate holds the harness until the bridge decides, then lets it start
// only on admission. Anything else, a refusal or the pod ending, fails it, and
// with it the pod, before the harness runs.
func TestRunGate(t *testing.T) {
	cases := []struct {
		name    string
		answers []int // GET /admission, in order; the last repeats
		want    string
	}{
		{"admitted at once lets the harness start", []int{http.StatusOK}, ""},
		{"pending, then admitted, waits and lets it start", []int{http.StatusServiceUnavailable, http.StatusServiceUnavailable, http.StatusOK}, ""},
		{"refused fails the pod with the reason", []int{http.StatusServiceUnavailable, http.StatusConflict}, wire.ReasonRoomBusy},
		{"never decided fails when the pod ends", []int{http.StatusServiceUnavailable}, context.DeadlineExceeded.Error()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			asked := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				code := c.answers[min(asked, len(c.answers)-1)]
				asked++
				mu.Unlock()
				if r.URL.Path != "/admission" {
					http.NotFound(w, r)
					return
				}
				w.WriteHeader(code)
				_, _ = w.Write([]byte(wire.ReasonRoomBusy + "\n"))
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
			defer cancel()
			err := gate(ctx, slog.New(slog.DiscardHandler), srv.URL+"/admission", 5*time.Millisecond)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("RunGate = %v, want the harness let through", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("RunGate = %v, want an error naming %q", err, c.want)
			}
			mu.Lock()
			defer mu.Unlock()
			if c.want == "" && asked < len(c.answers) {
				t.Fatalf("the gate asked %d times, want at least %d", asked, len(c.answers))
			}
		})
	}
}

// RunGate reads the bridge on loopback, at HEALTH_ADDR's port.
func TestRunGateReadsTheBridgeOnLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admission" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("admitted\n"))
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := RunGate(ctx, slog.New(slog.DiscardHandler), func(k string) string {
		return map[string]string{"HEALTH_ADDR": ":" + port}[k]
	}); err != nil {
		t.Fatalf("RunGate = %v, want the harness let through", err)
	}
}

func TestLoopback(t *testing.T) {
	for in, want := range map[string]string{":8085": "127.0.0.1:8085", "0.0.0.0:8085": "127.0.0.1:8085",
		"[::]:8085": "127.0.0.1:8085", "127.0.0.2:9": "127.0.0.2:9", "nonsense": "nonsense"} {
		if got := loopback(in); got != want {
			t.Errorf("loopback(%q) = %q, want %q", in, got, want)
		}
	}
}
