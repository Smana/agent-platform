// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
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
			noRead := func(context.Context) (bridge.FinalReadResult, error) { return bridge.FinalReadResult{}, nil }
			h := healthHandler(func(time.Time) bool { return healthy }, func() bridge.Admission { return bridge.Admission{} }, noRead, time.Now)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))
			if want := map[bool]int{true: http.StatusOK, false: http.StatusServiceUnavailable}[healthy]; rec.Code != want {
				t.Fatalf("GET /healthz = %d, want %d", rec.Code, want)
			}
		})
	}
}

// Review M7: a hook left unset fails open (OnReady, the steering gate) or
// wedges every run (OnStatus).
func TestWireBridgeSetsEveryHook(t *testing.T) {
	b := &bridge.Bridge{Harness: bridge.NewHarness("http://127.0.0.1:1", "c1"), RunID: "7f3cq2xz"}
	cfg := bridgeConfig{branch: "agent/3kq7x2ma", egress: map[string]bool{"npm": true}}
	confirm, steer := wireBridge(b, nil, cfg, slog.New(slog.DiscardHandler))
	hooks := map[string]bool{"OnDeliver": b.OnDeliver != nil, "OnInterrupt": b.OnInterrupt != nil,
		"OnResume": b.OnResume != nil, "OnReady": b.OnReady != nil, "OnRaw": b.OnRaw != nil,
		"OnStatus": b.OnStatus != nil, "OnDecision": b.OnDecision != nil, "Classify": b.Classify != nil,
		"steering gate": steer.Gate != nil, "confirmer": confirm != nil}
	for name, set := range hooks {
		if !set {
			t.Errorf("%s is not wired", name)
		}
	}
	for cmd, want := range map[string]string{"git push origin agent/3kq7x2ma": "forge.push", "npm install x": ""} {
		if got := b.Classify("terminal", json.RawMessage(`{"command":"`+cmd+`"}`), "LOW"); got != want {
			t.Errorf("%s classifies as %q, want %q: the branch and egress reach the classifier", cmd, got, want)
		}
	}
}

// Finding A: a composition that never set BRANCH made every push of run
// ikely2yk forge.other, silently. The bridge still starts, and says why.
func TestAnUnsetBranchIsLoggedAtStart(t *testing.T) {
	for branch, warned := range map[string]bool{"": true, "agent/3kq7x2ma": false} {
		var buf strings.Builder
		b := &bridge.Bridge{Harness: bridge.NewHarness("http://127.0.0.1:1", "c1"), RunID: "7f3cq2xz"}
		wireBridge(b, nil, bridgeConfig{branch: branch}, slog.New(slog.NewTextHandler(&buf, nil)))
		if got := strings.Contains(buf.String(), "level=WARN") && strings.Contains(buf.String(), "BRANCH"); got != warned {
			t.Errorf("branch %q: warned %v, want %v: %s", branch, got, warned, buf.String())
		}
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
			noRead := func(context.Context) (bridge.FinalReadResult, error) { return bridge.FinalReadResult{}, nil }
			h := healthHandler(func(time.Time) bool { return true }, func() bridge.Admission { return c.a }, noRead, time.Now)
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
		{"a bridge without /admission holds the gate", []int{http.StatusNotFound}, context.DeadlineExceeded.Error()},
		{"a bridge error holds the gate", []int{http.StatusInternalServerError}, context.DeadlineExceeded.Error()},
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

// F15: a bridge that is not listening (not started yet, or restarted after a
// liveness kill or an OOM) holds the harness; the gate never fails open.
func TestRunGateFailsClosedWhileTheBridgeIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL + "/admission"
	srv.Close() // connection refused from here on
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if err := gate(ctx, slog.New(slog.DiscardHandler), url, 5*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("gate = %v, want it held until the pod ends", err)
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

// F11, the harness half: agent-run asks for the final read on loopback, POST only, and waits at
// most finalReadWait for the answer.
func TestFinalReadEndpoint(t *testing.T) {
	read := func(context.Context) (bridge.FinalReadResult, error) { return bridge.FinalReadResult{Events: 7}, nil }
	never := func(ctx context.Context) (bridge.FinalReadResult, error) {
		<-ctx.Done()
		return bridge.FinalReadResult{}, ctx.Err()
	}
	for _, c := range []struct {
		name, method, from string
		read               func(context.Context) (bridge.FinalReadResult, error)
		code               int
		body               string
	}{
		{"the harness gets the answer", http.MethodPost, "127.0.0.1:41234", read, http.StatusOK, `{"events":7,"unmirrored":0,"sealed":false}`},
		{"loopback only", http.MethodPost, "10.0.0.7:41234", read, http.StatusForbidden, "loopback only"},
		{"POST only", http.MethodGet, "127.0.0.1:41234", read, http.StatusMethodNotAllowed, ""},
		{"a read that never answers is a 504", http.MethodPost, "127.0.0.1:41234", never, http.StatusGatewayTimeout, "in time"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := healthHandler(func(time.Time) bool { return true }, func() bridge.Admission { return bridge.Admission{} }, c.read, time.Now)
			req := httptest.NewRequestWithContext(t.Context(), c.method, "/final-read", nil)
			req.RemoteAddr = c.from
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.body) {
				t.Fatalf("%s /final-read from %s = %d %q, want %d %q", c.method, c.from, rec.Code, rec.Body.String(), c.code, c.body)
			}
		})
	}
}
