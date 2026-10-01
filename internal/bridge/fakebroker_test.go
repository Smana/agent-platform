// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/Smana/agent-platform/internal/wire"
)

// reply is one scripted broker answer; the zero value is not used.
type reply struct {
	code       int
	reason     string
	retryAfter string
}

// fakeBroker is :8443 as internal/bridgeapi serves it, over TLS: hello, events
// and the stream. Scripted replies are consumed in order; once they run out,
// every request is accepted. maxBody, when set, answers 413 to a larger body,
// as the broker's MaxBytesReader does.
type fakeBroker struct {
	mu      sync.Mutex
	items   []wire.Item
	tokens  []string
	calls   []string // "hello 200", "events 409", …
	posts   []time.Time
	bodies  []int
	resume  wire.Resume
	hellos  []reply
	events  []reply
	maxBody int
	hold    bool     // answer 503 to every events POST while set
	poison  string   // a batch holding an item whose payload contains it is 400 bad_item
	sse     []string // frames the stream sends before idling
	// approvals are the approval requests received; each opens "ap-<callId>".
	approvals []wire.ApprovalRequest
	// conflicts are seqs pushed again with other content than the log holds.
	conflicts []string
}

func (b *fakeBroker) record(r *http.Request, what string, code int) {
	b.tokens = append(b.tokens, r.Header.Get("Authorization"))
	b.calls = append(b.calls, what+" "+http.StatusText(code))
}

func refuse(w http.ResponseWriter, rp reply) {
	if rp.retryAfter != "" {
		w.Header().Set("Retry-After", rp.retryAfter)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rp.code)
	_ = json.NewEncoder(w).Encode(wire.Error{Reason: rp.reason})
}

// start serves the fake over TLS and writes its CA where the bridge reads it.
func (b *fakeBroker) start(t *testing.T) (srv *httptest.Server, caFile string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/bridge/hello", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		if len(b.hellos) > 0 {
			rp := b.hellos[0]
			b.hellos = b.hellos[1:]
			b.record(r, "hello", rp.code)
			if rp.code != http.StatusOK {
				refuse(w, rp)
				return
			}
			_ = json.NewEncoder(w).Encode(b.resume)
			return
		}
		b.record(r, "hello", http.StatusOK)
		_ = json.NewEncoder(w).Encode(b.resume)
	})
	mux.HandleFunc("POST /v1/bridge/events", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		defer b.mu.Unlock()
		b.posts = append(b.posts, time.Now())
		b.bodies = append(b.bodies, len(body))
		switch {
		case len(b.events) > 0:
			rp := b.events[0]
			b.events = b.events[1:]
			b.record(r, "events", rp.code)
			refuse(w, rp)
			return
		case b.hold:
			b.record(r, "events", http.StatusServiceUnavailable)
			refuse(w, reply{code: http.StatusServiceUnavailable, reason: wire.ReasonLogUnavailable})
			return
		case b.maxBody > 0 && len(body) > b.maxBody:
			b.record(r, "events", http.StatusRequestEntityTooLarge)
			refuse(w, reply{code: http.StatusRequestEntityTooLarge, reason: wire.ReasonBatchTooLarge})
			return
		}
		var batch wire.Batch
		if err := json.Unmarshal(body, &batch); err != nil {
			b.record(r, "events", http.StatusBadRequest)
			refuse(w, reply{code: http.StatusBadRequest, reason: wire.ReasonBadBatch})
			return
		}
		for _, it := range batch.Items {
			if b.poison != "" && strings.Contains(string(it.Payload), b.poison) {
				b.record(r, "events", http.StatusBadRequest)
				refuse(w, reply{code: http.StatusBadRequest, reason: wire.ReasonBadItem})
				return
			}
		}
		b.record(r, "events", http.StatusOK)
		for _, it := range batch.Items {
			// The store's idempotency: a seq already held is dropped, and one
			// held with other content is a bug in the bridge.
			i := slices.IndexFunc(b.items, func(o wire.Item) bool { return o.Stream == it.Stream && o.Seq == it.Seq })
			switch {
			case i < 0:
				b.items = append(b.items, it)
			case b.items[i].Type != it.Type || !bytes.Equal(b.items[i].Payload, it.Payload):
				b.conflicts = append(b.conflicts, fmt.Sprintf("%s %d", it.Stream, it.Seq))
			}
		}
		_ = json.NewEncoder(w).Encode(wire.BatchAck{})
	})
	mux.HandleFunc("POST /v1/bridge/approvals", func(w http.ResponseWriter, r *http.Request) {
		var req wire.ApprovalRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.CallID == "" || !json.Valid(req.Action) {
			refuse(w, reply{code: http.StatusBadRequest, reason: "bad_approval"})
			return
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		b.record(r, "approvals", http.StatusOK)
		b.approvals = append(b.approvals, req)
		_ = json.NewEncoder(w).Encode(wire.ApprovalAck{ApprovalID: "ap-" + req.CallID, ExpiresAt: time.Now().Add(time.Hour)})
	})
	mux.HandleFunc("GET /v1/bridge/stream", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		frames := append([]string{}, b.sse...)
		b.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	caFile = filepath.Join(t.TempDir(), "ca.crt")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caFile, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return srv, caFile
}

func (b *fakeBroker) set(f func(b *fakeBroker)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	f(b)
}

// stored is what the log holds per stream, keyed by seq, in arrival order.
func (b *fakeBroker) stored(st wire.Stream) []wire.Item {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []wire.Item
	for _, it := range b.items {
		if it.Stream == st {
			out = append(out, it)
		}
	}
	return out
}

func (b *fakeBroker) callLog() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string{}, b.calls...)
}

func writeToken(t *testing.T, dir, value string) string {
	t.Helper()
	p := filepath.Join(dir, "token")
	if err := os.WriteFile(p, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// lockedBuffer is a log sink the test reads while the bridge writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// rig is a bridge wired to a fake harness and a fake broker, with fast timings,
// a captured log and a readable meter.
type rig struct {
	cancel context.CancelFunc // SIGTERM: Run drains, then closes done
	done   chan struct{}
	b      *Bridge
	fb     *fakeBroker
	logs   *lockedBuffer
	reader *sdkmetric.ManualReader
	tokDir string
}

func newRig(t *testing.T, h *Harness, fb *fakeBroker) *rig {
	t.Helper()
	srv, ca := fb.start(t)
	dir := t.TempDir()
	br, err := NewBroker(srv.URL, writeToken(t, dir, "v1"), ca)
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	reader := sdkmetric.NewManualReader()
	b := &Bridge{Harness: h, Broker: br, RunID: runID, Interval: 5 * time.Millisecond, MaxBuffer: 1 << 20,
		MinBackoff: time.Millisecond, MaxBackoff: 20 * time.Millisecond, FlushGrace: time.Second,
		Logger: slog.New(slog.NewJSONHandler(logs, nil)),
		Meter:  sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")}
	t.Cleanup(func() {
		fb.mu.Lock()
		defer fb.mu.Unlock()
		if len(fb.conflicts) > 0 {
			t.Errorf("seqs pushed again with other content: %v", fb.conflicts)
		}
	})
	return &rig{b: b, fb: fb, logs: logs, reader: reader, tokDir: dir}
}

// run starts the bridge under a 5 s deadline and returns a stop that cancels it
// and waits for Run to return, so a regression fails fast instead of hanging.
func (r *rig) run(t *testing.T) (ctx context.Context, stop func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	done := make(chan struct{})
	r.cancel, r.done = cancel, done
	go func() {
		defer close(done)
		_ = r.b.Run(ctx)
	}()
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		<-done
	}
	t.Cleanup(stop)
	return ctx, stop
}

// eventually polls cond until it holds or ctx ends.
func eventually(ctx context.Context, t *testing.T, what string, cond func() bool) {
	t.Helper()
	for !cond() {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting until %s", what)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// counter reads an int64 counter's value for one reason label.
func (r *rig) counter(t *testing.T, name, reason string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.reader.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if m.Name != name || !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				if v, ok := dp.Attributes.Value("reason"); ok && v.AsString() == reason {
					return dp.Value
				}
			}
		}
	}
	return 0
}

func payloadHas(it wire.Item, s string) bool { return strings.Contains(string(it.Payload), s) }
