// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/fanout"
	"github.com/Smana/agent-platform/internal/humanapi/ui"
	"github.com/Smana/agent-platform/internal/metrics"
	"github.com/Smana/agent-platform/internal/policy"
	"github.com/Smana/agent-platform/internal/runwatch"
	"github.com/Smana/agent-platform/internal/store"
	"github.com/Smana/agent-platform/internal/wire"
)

const (
	roomID    = "3kq7x2ma"
	namespace = "agent-system"
	member    = "agents-member"
)

var groups = policy.Groups{Admin: "agents-admin", Member: member}

// memLog is the room's log. roomErr, when set, fails every Room read;
// appendAfter, when set, appends one event just after the Room read of that
// number (1-based, the hub's reads included) has taken its mark; from read
// driverFrom on, driver is the room's driver.
type memLog struct {
	mu          sync.Mutex
	evs         []envelope.Event
	roomErr     error
	appendAfter int
	driverFrom  int
	driver      string
	roomReads   int
}

func (m *memLog) add(n int) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.addLocked(n)
}

func (m *memLog) addLocked(n int) int64 {
	for range n {
		m.evs = append(m.evs, envelope.Event{V: 1, Seq: int64(len(m.evs) + 1), RoomID: roomID,
			Type: envelope.Message, Payload: []byte(`{"kind":"chat","text":"x","delivery":"none"}`), TS: time.Now()})
	}
	return int64(len(m.evs))
}

func (m *memLog) Range(_ context.Context, _ string, after int64, limit int) ([]envelope.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []envelope.Event
	for _, e := range m.evs {
		if e.Seq > after && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (m *memLog) Room(_ context.Context, id string) (store.RoomState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.roomErr != nil {
		return store.RoomState{}, m.roomErr
	}
	m.roomReads++
	st := store.RoomState{ID: id, LastSeq: int64(len(m.evs)), Driver: "system:factory", DriverEpoch: 3}
	if m.driverFrom > 0 && m.roomReads >= m.driverFrom {
		st.Driver = m.driver
	}
	if m.roomReads == m.appendAfter {
		m.addLocked(1)
	}
	return st, nil
}

// countingHub counts the subscriptions a Server holds open.
type countingHub struct {
	Hub
	open atomic.Int64
}

func (h *countingHub) Subscribe(ctx context.Context, room string) (*fanout.Sub, error) {
	sub, err := h.Hub.Subscribe(ctx, room)
	if err == nil {
		h.open.Add(1)
	}
	return sub, err
}

func (h *countingHub) Unsubscribe(sub *fanout.Sub) {
	h.open.Add(-1)
	h.Hub.Unsubscribe(sub)
}

// hubView is the hub's read of the log. It never returns seq skip, as a read
// racing a slow commit would not, so the live stream has a gap there.
type hubView struct {
	*memLog
	skip int64
}

func (v hubView) Range(ctx context.Context, room string, after int64, limit int) ([]envelope.Event, error) {
	evs, err := v.memLog.Range(ctx, room, after, limit)
	out := evs[:0:0]
	for _, e := range evs {
		if e.Seq != v.skip {
			out = append(out, e)
		}
	}
	return out, err
}

// headerAuth authenticates X-Test-User as human:<user>, an agents-member.
// X-Test-TTL sets the token's remaining life, X-Test-Kind the principal's kind,
// X-Test-Forbidden refuses as a foreign Origin would.
type headerAuth struct{}

func (headerAuth) Authenticate(r *http.Request) (authn.Principal, error) {
	if r.Header.Get("X-Test-Forbidden") != "" {
		return authn.Principal{}, authn.ErrForbidden
	}
	who := r.Header.Get("X-Test-User")
	if who == "" {
		return authn.Principal{}, authn.ErrUnauthenticated
	}
	gs := []string{member}
	if who == "stranger" {
		gs = []string{"backend"}
	}
	ttl := time.Hour
	if v := r.Header.Get("X-Test-TTL"); v != "" {
		ttl, _ = time.ParseDuration(v)
	}
	kind := envelope.ActorHuman
	if v, ok := r.Header["X-Test-Kind"]; ok {
		kind = envelope.ActorKind(v[0])
	}
	return authn.Principal{Kind: kind, ID: "human:" + who, Sub: who, Groups: gs, ClientID: "web",
		Expiry: time.Now().Add(ttl)}, nil
}

type env struct {
	ts   *httptest.Server
	srv  *Server
	log  *memLog
	hub  *fanout.Hub
	subs *countingHub
	exp  *metrics.Exporter
}

type option func(*Server, *fanout.Hub, *hubView)

func setup(t *testing.T, opts ...option) env {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	room := &v1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: roomID, Namespace: namespace},
		Spec:   v1alpha1.RoomSpec{Owner: "human:own", Driver: "system:factory", DataClass: "public"},
		Status: v1alpha1.RoomStatus{Phase: "Active", LastSeq: 10, Driver: "system:factory"}}
	elsewhere := &v1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: "9zz9zz9z", Namespace: "other"},
		Spec: v1alpha1.RoomSpec{Owner: "human:own", Driver: "system:factory", DataClass: "public"}}
	log := &memLog{}
	log.add(10)
	view := &hubView{memLog: log}
	hub := fanout.New(view, nil, nil)
	hub.PollEvery = 20 * time.Millisecond
	exp, err := metrics.NewExporter("test")
	if err != nil {
		t.Fatal(err)
	}
	m, err := metrics.New(exp.Meter())
	if err != nil {
		t.Fatal(err)
	}
	runs := runwatch.New()
	subs := &countingHub{Hub: hub}
	srv := &Server{Humans: headerAuth{}, Groups: groups, WebClient: func() string { return "web" },
		Rooms: fake.NewClientBuilder().WithScheme(s).WithObjects(room, elsewhere).Build(), Namespace: namespace,
		Log: log, Hub: subs, Runs: runs, Metrics: m}
	for _, o := range opts {
		o(srv, hub, view)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- hub.Run(ctx) }()
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(func() {
		ts.CloseClientConnections()
		srv.closeAll()
		ts.Close()
		cancel()
		<-done
		_ = exp.Shutdown(context.Background())
	})
	return env{ts: ts, srv: srv, log: log, hub: hub, subs: subs, exp: exp}
}

func wsURL(ts *httptest.Server) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/ws?room=" + roomID
}

func header(user string, kv ...string) http.Header {
	h := http.Header{}
	if user != "" {
		h.Set("X-Test-User", user)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func connect(t *testing.T, e env, h http.Header) (*websocket.Conn, int, error) {
	t.Helper()
	return connectTo(t, wsURL(e.ts), h)
}

// connectTo dials u and returns the connection, or the status that refused it.
func connectTo(t *testing.T, u string, h http.Header) (*websocket.Conn, int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: h})
	if c != nil {
		t.Cleanup(func() { _ = c.CloseNow() })
	}
	code := 0
	if resp != nil {
		code = resp.StatusCode
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	return c, code, err
}

// dial opens a connection and sends hello.
func dial(t *testing.T, e env, user string, hello wire.ClientFrame) *websocket.Conn {
	t.Helper()
	c, _, err := connect(t, e, header(user))
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, hello)
	return c
}

func send(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, c, v); err != nil {
		t.Fatal(err)
	}
}

func hello(after *int64, tail int) wire.ClientFrame {
	return wire.ClientFrame{Type: wire.FrameHello, RoomID: roomID, AfterSeq: after, Tail: tail}
}

func read(t *testing.T, c *websocket.Conn) wire.ServerFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var f wire.ServerFrame
	if err := wsjson.Read(ctx, c, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// closed reads until the connection ends and returns how the broker closed it.
func closed(t *testing.T, c *websocket.Conn) (websocket.StatusCode, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		_, _, err := c.Read(ctx)
		if err == nil {
			continue
		}
		var ce websocket.CloseError
		if errors.As(err, &ce) {
			return ce.Code, ce.Reason
		}
		return -1, err.Error()
	}
}

func events(t *testing.T, c *websocket.Conn, n int) []int64 {
	t.Helper()
	var seqs []int64
	for range n {
		f := read(t, c)
		if f.Type != wire.FrameEvent {
			t.Fatalf("got %+v, want an event", f)
		}
		seqs = append(seqs, f.Event.Seq)
	}
	return seqs
}

func consecutive(t *testing.T, seqs []int64, from int64) {
	t.Helper()
	for i, s := range seqs {
		if s != from+int64(i) {
			t.Fatalf("seqs %v, want consecutive from %d", seqs, from)
		}
	}
}

func scrape(t *testing.T, e env) string {
	t.Helper()
	rec := httptest.NewRecorder()
	e.exp.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	b, _ := io.ReadAll(rec.Body)
	return string(b)
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// SC-2 offline: replay from afterSeq, then live, with no gap and no duplicate.
func TestReplayThenLive(t *testing.T) {
	e := setup(t)
	after := int64(4)
	c := dial(t, e, "dev", hello(&after, 0))
	f := read(t, c)
	if f.Type != wire.FrameState || f.ThroughSeq != 10 || f.Snapshot.You.Role != "watcher" || !f.Snapshot.You.WebUI ||
		f.Snapshot.Driver != "system:factory" || f.Snapshot.DriverEpoch != 3 || f.Snapshot.DataClass != "public" || f.Snapshot.Phase != "Active" {
		t.Fatalf("state = %+v %+v", f, f.Snapshot)
	}
	if f := read(t, c); f.Type != wire.FrameSync || f.FromSeq != 5 || f.ThroughSeq != 10 {
		t.Fatalf("sync = %+v", f)
	}
	seqs := events(t, c, 6)
	e.log.add(3) // appended elsewhere: the hub's poll finds it
	consecutive(t, append(seqs, events(t, c, 3)...), 5)
}

func TestWhereReplayStarts(t *testing.T) {
	neg, far := int64(-3), int64(99)
	cases := []struct {
		name          string
		hello         wire.ClientFrame
		from, through int64
	}{
		{"tail 3 of 10 syncs from 8", hello(nil, 3), 8, 10},
		{"a tail longer than the log starts at 1", hello(nil, 50), 1, 10},
		{"no afterSeq and no tail: the last 500", hello(nil, 0), 1, 10},
		{"a negative afterSeq starts at 1", hello(&neg, 0), 1, 10},
		{"an afterSeq past the mark is clamped to it", hello(&far, 0), 11, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			c := dial(t, e, "dev", tc.hello)
			read(t, c)
			if f := read(t, c); f.Type != wire.FrameSync || f.FromSeq != tc.from || f.ThroughSeq != tc.through {
				t.Fatalf("sync = %+v", f)
			}
		})
	}
}

// §4: a gap in the live seqs is filled from the log before the next event.
func TestALiveGapIsReadFromTheLog(t *testing.T) {
	e := setup(t, func(_ *Server, _ *fanout.Hub, v *hubView) { v.skip = 12 })
	after := int64(10)
	c := dial(t, e, "dev", hello(&after, 0))
	read(t, c)
	read(t, c)
	e.log.add(1)
	consecutive(t, events(t, c, 1), 11)
	e.log.add(2) // the hub never sees 12
	if f := read(t, c); f.Type != wire.FrameSync || f.FromSeq != 12 || f.ThroughSeq != 12 {
		t.Fatalf("sync = %+v", f)
	}
	consecutive(t, events(t, c, 2), 12)
}

// Everything refused before the upgrade is a plain HTTP status (docs/api.md).
func TestRefusedBeforeTheUpgrade(t *testing.T) {
	cases := []struct {
		name string
		h    http.Header
		fail func(*memLog)
		want int
	}{
		{"no credential", header(""), nil, http.StatusUnauthorized},
		{"a token past its expiry, inside the verifier's leeway", header("dev", "X-Test-TTL", "-1s"), nil, http.StatusUnauthorized},
		{"a foreign Origin, refused by Authenticate", header("dev", "X-Test-Forbidden", "1"), nil, http.StatusForbidden},
		{"a foreign Origin at the upgrade (T9)", header("dev", "Origin", "https://evil.example"), nil, http.StatusForbidden},
		{"an opaque Origin at the upgrade", header("dev", "Origin", "null"), nil, http.StatusForbidden},
		{"a user outside the agents groups", header("stranger"), nil, http.StatusForbidden},
		{"a principal of no known kind may not read", header("dev", "X-Test-Kind", ""), nil, http.StatusForbidden},
		{"the log is unavailable", header("dev"), func(m *memLog) { m.roomErr = errors.New("down") }, http.StatusServiceUnavailable},
		{"the room has no log row", header("dev"), func(m *memLog) { m.roomErr = store.ErrNoRoom }, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			if tc.fail != nil {
				tc.fail(e.log)
			}
			if _, code, err := connect(t, e, tc.h); err == nil || code != tc.want {
				t.Fatalf("got %d, %v; want %d", code, err, tc.want)
			}
		})
	}
	for name, room := range map[string]string{"an unknown room": "4kq7x2ma", "a room in another namespace": "9zz9zz9z",
		"not a room id": "..%2Fx", "no room": ""} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			u := "ws" + strings.TrimPrefix(e.ts.URL, "http") + "/v1/ws?room=" + room
			if _, code, err := connectTo(t, u, header("dev")); err == nil || code != http.StatusNotFound {
				t.Fatalf("got %d, %v; want 404", code, err)
			}
		})
	}
}

// §4, ruling P22: 10 connections per person, 20 people per room, per replica.
func TestConnectionLimits(t *testing.T) {
	t.Run("ten per person", func(t *testing.T) {
		e := setup(t)
		var conns []*websocket.Conn
		for range maxPerUser {
			c, _, err := connect(t, e, header("dev"))
			if err != nil {
				t.Fatal(err)
			}
			conns = append(conns, c)
		}
		if _, code, err := connect(t, e, header("dev")); err == nil || code != http.StatusTooManyRequests {
			t.Fatalf("the 11th: %d %v", code, err)
		}
		_ = conns[0].Close(websocket.StatusNormalClosure, "")
		eventually(t, "a closed connection frees its slot", func() bool {
			c, _, err := connect(t, e, header("dev"))
			if c != nil {
				_ = c.CloseNow()
			}
			return err == nil
		})
	})
	t.Run("twenty people per room, and one more tab for someone already in", func(t *testing.T) {
		e := setup(t)
		for i := range maxPerRoom {
			if _, _, err := connect(t, e, header("u"+string(rune('a'+i)))); err != nil {
				t.Fatal(err)
			}
		}
		if _, code, err := connect(t, e, header("late")); err == nil || code != http.StatusTooManyRequests {
			t.Fatalf("the 21st person: %d %v", code, err)
		}
		if _, _, err := connect(t, e, header("ua")); err != nil {
			t.Fatalf("a second tab: %v", err)
		}
	})
}

func TestHelloFirst(t *testing.T) {
	for name, first := range map[string]wire.ClientFrame{
		"a ping":             {Type: wire.FramePing},
		"hello another room": {Type: wire.FrameHello, RoomID: "4kq7x2ma"},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			c := dial(t, e, "dev", first)
			if code, reason := closed(t, c); code != websocket.StatusPolicyViolation || reason != "hello first" {
				t.Fatalf("closed %d %q", code, reason)
			}
			protocolDrop(t, e)
		})
	}
	t.Run("no hello at all", func(t *testing.T) {
		e := setup(t, func(s *Server, _ *fanout.Hub, _ *hubView) { s.HelloWait = 50 * time.Millisecond })
		c, _, err := connect(t, e, header("dev"))
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		closed(t, c)
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("a silent client held its slot for %s", d)
		}
		protocolDrop(t, e)
	})
}

// A token that expires before any hello is the token's end, not the client's
// fault: reauth, never protocol (review G1).
func TestATokenExpiringBeforeTheHelloIsReauth(t *testing.T) {
	e := setup(t, func(s *Server, _ *fanout.Hub, _ *hubView) { s.HelloWait = 5 * time.Second })
	c, _, err := connect(t, e, header("dev", "X-Test-TTL", "200ms"))
	if err != nil {
		t.Fatal(err)
	}
	closed(t, c)
	eventually(t, "the drop is counted as reauth", func() bool {
		return strings.Contains(scrape(t, e), `rooms_connections_dropped_total{reason="reauth"} 1`)
	})
	if body := scrape(t, e); strings.Contains(body, `reason="protocol"`) {
		t.Fatal("counted as a protocol drop")
	}
}

// The connection lives min(token exp, 1 h), then closes 4001 for re-authentication.
func TestReauth(t *testing.T) {
	e := setup(t)
	c, _, err := connect(t, e, header("dev", "X-Test-TTL", "300ms"))
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, hello(nil, 0))
	if code, reason := closed(t, c); code != closeReauth || reason != "reauth" {
		t.Fatalf("closed %d %q", code, reason)
	}
	eventually(t, "the drop is counted", func() bool {
		return strings.Contains(scrape(t, e), `rooms_connections_dropped_total{reason="reauth"} 1`)
	})
}

// §4: a viewer that falls over its budget is dropped, and resumes from afterSeq.
func TestSlowConsumerIsDropped(t *testing.T) {
	e := setup(t, func(_ *Server, h *fanout.Hub, _ *hubView) { h.Budget = 100 })
	c := dial(t, e, "dev", hello(nil, 0))
	read(t, c)
	read(t, c)
	e.log.add(50) // one burst: the hub offers them all before the socket takes two
	if code, reason := closed(t, c); code != websocket.StatusPolicyViolation || !strings.HasPrefix(reason, "slow_consumer") {
		t.Fatalf("closed %d %q", code, reason)
	}
	eventually(t, "the drop is counted", func() bool {
		return strings.Contains(scrape(t, e), `rooms_connections_dropped_total{reason="slow_consumer"} 1`)
	})
}

// A peer that stops answering pings is cut, so a dead TCP path frees its slot.
func TestUnansweredPingCutsTheConnection(t *testing.T) {
	e := setup(t, func(s *Server, _ *fanout.Hub, _ *hubView) {
		s.PingEvery, s.PongWait = 50*time.Millisecond, 50*time.Millisecond
	})
	c, _, err := connect(t, e, header("dev"))
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, hello(nil, 0)) // then never read, so never pong
	eventually(t, "the drop is counted", func() bool {
		return strings.Contains(scrape(t, e), `rooms_connections_dropped_total{reason="ping_timeout"} 1`)
	})
}

// A client frame over the read limit closes the connection 1009.
func TestOversizeFrame(t *testing.T) {
	e := setup(t)
	c := dial(t, e, "dev", hello(nil, 0))
	read(t, c)
	read(t, c)
	for range 10 {
		read(t, c)
	}
	send(t, c, wire.ClientFrame{Type: wire.FrameAct, Action: json.RawMessage(`"` + strings.Repeat("a", maxClientFrame) + `"`)})
	if code, _ := closed(t, c); code != websocket.StatusMessageTooBig {
		t.Fatalf("closed %d", code)
	}
	protocolDrop(t, e)
}

// A frame that is not a client frame closes the connection 1007, as the broker's drop.
func TestMalformedFrame(t *testing.T) {
	for name, tc := range map[string]struct {
		first bool
		frame string
	}{
		"not JSON, as the hello": {true, "{"},
		"not JSON, after it":     {false, "{"},
		"a mistyped field":       {false, `{"type":1}`},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			c, _, err := connect(t, e, header("dev"))
			if err != nil {
				t.Fatal(err)
			}
			if !tc.first {
				send(t, c, hello(nil, 0))
				read(t, c)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := c.Write(ctx, websocket.MessageText, []byte(tc.frame)); err != nil {
				t.Fatal(err)
			}
			if code, _ := closed(t, c); code != websocket.StatusInvalidFramePayloadData {
				t.Fatalf("closed %d", code)
			}
			protocolDrop(t, e)
		})
	}
}

// protocolDrop waits for the one close counted as the client breaking protocol (review R2).
func protocolDrop(t *testing.T, e env) {
	t.Helper()
	eventually(t, "the protocol drop is counted", func() bool {
		return strings.Contains(scrape(t, e), `rooms_connections_dropped_total{reason="protocol"} 1`)
	})
}

// failed keeps the broker's own cause: a timed-out write is write_timeout while the
// connection lives or when the reader saw the socket it tore down go, the log is
// log_unavailable while alive, and any other cause wins over both (review R1).
func TestFailedNamesTheBrokersCause(t *testing.T) {
	timedOut := errors.Join(errWriteTimeout, context.DeadlineExceeded)
	logDown := errors.Join(errLog, errors.New("conn refused"))
	for name, tc := range map[string]struct {
		err   error
		cause error
		want  string
	}{
		"a write timeout, alive":                    {timedOut, nil, dropWriteTimeout},
		"a write timeout the reader saw as the end": {timedOut, errClientGone, dropWriteTimeout},
		"a write timeout during shutdown":           {timedOut, errShutdown, dropShutdown},
		"a write timeout after a ping timeout":      {timedOut, errPingTimeout, dropPingTimeout},
		"the log, alive":                            {logDown, nil, dropLogUnavailable},
		"the log, once the peer left":               {logDown, errClientGone, dropClientGone},
		"another write error, the peer gone":        {errors.New("broken pipe"), errClientGone, dropClientGone},
	} {
		t.Run(name, func(t *testing.T) {
			life, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			if tc.cause != nil {
				cancel(tc.cause)
			}
			v := &viewer{s: &Server{}, life: life}
			if got := v.failed(tc.err); got != tc.want {
				t.Fatalf("failed = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestActs(t *testing.T) {
	act := wire.ClientFrame{Type: wire.FrameAct, ClientSeq: 7, Action: json.RawMessage(`{"type":"message"}`)}
	t.Run("refused while no phase handles them", func(t *testing.T) {
		e := setup(t)
		c := dial(t, e, "dev", hello(nil, 0))
		read(t, c)
		read(t, c)
		events(t, c, 10)
		send(t, c, act)
		if f := read(t, c); f.Type != wire.FrameAck || f.ClientSeq != 7 || f.Rejected != wire.ReasonNotPermitted {
			t.Fatalf("ack = %+v", f)
		}
	})
	t.Run("handed to the handler with the caller and the room", func(t *testing.T) {
		var got atomic.Value
		e := setup(t, func(s *Server, _ *fanout.Hub, _ *hubView) {
			s.Acts = func(_ context.Context, p authn.Principal, room *v1alpha1.Room, f wire.ClientFrame) wire.ServerFrame {
				got.Store(p.ID + " " + room.Name)
				return wire.ServerFrame{Type: wire.FrameAck, ClientSeq: f.ClientSeq, Seq: 11}
			}
		})
		c := dial(t, e, "dev", hello(nil, 0))
		read(t, c)
		read(t, c)
		events(t, c, 10)
		send(t, c, act)
		if f := read(t, c); f.Type != wire.FrameAck || f.ClientSeq != 7 || f.Seq != 11 || got.Load() != "human:dev "+roomID {
			t.Fatalf("ack = %+v, handler saw %v", f, got.Load())
		}
	})
}

// Shutdown ends every connection 1001 at once, rather than hold the drain open.
func TestServeClosesConnectionsOnShutdown(t *testing.T) {
	e := setup(t)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- e.srv.Serve(ctx, ln, 5*time.Second) }()
	c, _, err := connectTo(t, "ws://"+ln.Addr().String()+"/v1/ws?room="+roomID, header("dev"))
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, hello(nil, 0))
	read(t, c)
	eventually(t, "the connection is counted", func() bool {
		return strings.Contains(scrape(t, e), `rooms_connections{kind="human"} 1`)
	})
	start := time.Now()
	cancel()
	if code, reason := closed(t, c); code != websocket.StatusGoingAway || reason != "shutdown" {
		t.Fatalf("closed %d %q", code, reason)
	}
	if err := <-served; err != nil || time.Since(start) > 3*time.Second {
		t.Fatalf("serve: %v after %s", err, time.Since(start))
	}
	eventually(t, "the connection is uncounted", func() bool {
		return strings.Contains(scrape(t, e), `rooms_connections{kind="human"} 0`)
	})
}

// Handlers that outlast the drain are named, not dropped silently: their viewers
// see 1006, not 1001 (review G7). A peer that never reads never answers the close
// handshake, which holds its handler past a short drain.
func TestServeNamesConnectionsCutByTheDrain(t *testing.T) {
	e := setup(t)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- e.srv.Serve(ctx, ln, 200*time.Millisecond) }()
	c, _, err := connectTo(t, "ws://"+ln.Addr().String()+"/v1/ws?room="+roomID, header("dev"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.CloseNow() }()
	send(t, c, hello(nil, 0))
	eventually(t, "the connection is counted", func() bool {
		return strings.Contains(scrape(t, e), `rooms_connections{kind="human"} 1`)
	})
	cancel()
	if err := <-served; err == nil || !strings.Contains(err.Error(), "1 WebSocket connection") {
		t.Fatalf("serve = %v, want the cut connection named", err)
	}
}

// A connection that races the shutdown is closed 1001 rather than kept.
func TestNoConnectionAfterShutdown(t *testing.T) {
	e := setup(t)
	e.srv.closeAll()
	c, _, err := connect(t, e, header("dev"))
	if err != nil {
		t.Fatal(err)
	}
	if code, reason := closed(t, c); code != websocket.StatusGoingAway || reason != "shutdown" {
		t.Fatalf("closed %d %q", code, reason)
	}
}

// Live events at or below a viewer's mark were replayed from the log: never twice.
func TestNothingTwiceAtTheMark(t *testing.T) {
	poll := make(chan time.Time)
	e := setup(t, func(_ *Server, h *fanout.Hub, _ *hubView) {
		h.PollEvery = time.Hour
		h.After = func(d time.Duration) <-chan time.Time {
			if d == time.Hour {
				return poll
			}
			return time.After(d)
		}
	})
	a := dial(t, e, "a", hello(nil, 0))
	read(t, a)
	read(t, a)
	events(t, a, 10)
	e.log.add(3) // the hub has not read them yet
	b := dial(t, e, "b", hello(nil, 0))
	if f := read(t, b); f.ThroughSeq != 13 {
		t.Fatalf("state = %+v", f)
	}
	read(t, b)
	consecutive(t, events(t, b, 13), 1)
	poll <- time.Now() // the hub now offers 11..13 to both
	consecutive(t, events(t, a, 3), 11)
	e.log.add(1)
	poll <- time.Now()
	consecutive(t, events(t, b, 1), 14)
}

func TestRoomList(t *testing.T) {
	e := setup(t)
	get := func(h http.Header) (int, []byte) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, e.ts.URL+"/api/rooms", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header = h
		r, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Body.Close() }()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, b
	}
	for user, want := range map[string]int{"": http.StatusUnauthorized, "stranger": http.StatusForbidden} {
		if code, _ := get(header(user)); code != want {
			t.Fatalf("%q: %d, want %d", user, code, want)
		}
	}
	code, body := get(header("dev"))
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil || code != http.StatusOK {
		t.Fatalf("%d %v", code, err)
	}
	want := `[{"dataClass":"public","driver":"system:factory","id":"3kq7x2ma","lastSeq":10,"owner":"human:own","phase":"Active",` +
		`"you":{"approver":false,"driver":false,"principal":"human:dev","role":"watcher","webUI":true}}]`
	if b, _ := json.Marshal(rows); string(b) != want {
		t.Fatalf("rows %s\nwant %s", b, want)
	}
	if _, b := get(header("dev", "X-Test-Kind", "")); strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("a principal who may read nothing lists %s", b)
	}
}

// T10: the UI is served under the strict CSP; without one (Task 2.5) it is 404.
func TestUI(t *testing.T) {
	ui := fstest.MapFS{"index.html": {Data: []byte("<html>")}, "app.js": {Data: []byte("js")}}
	e := setup(t, func(s *Server, _ *fanout.Hub, _ *hubView) { s.UI = ui })
	bare := setup(t)
	get := func(ts *httptest.Server, path string) (int, string, string) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		r, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Body.Close() }()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b), r.Header.Get("Content-Security-Policy")
	}
	for _, tc := range []struct {
		path string
		code int
		body string
	}{{"/", 200, "<html>"}, {"/r/" + roomID, 200, "<html>"}, {"/assets/app.js", 200, "js"}, {"/assets/nope.js", 404, ""}} {
		code, body, policy := get(e.ts, tc.path)
		if code != tc.code || (code == 200 && (body != tc.body || policy != csp)) {
			t.Fatalf("%s: %d %q csp %q", tc.path, code, body, policy)
		}
	}
	if code, _, _ := get(bare.ts, "/"); code != http.StatusNotFound {
		t.Fatalf("no UI: %d", code)
	}
}

// The committed bundle is served from the broker alone, under the strict CSP, with
// no inline script and every response's hardening headers (T10).
func TestTheBuiltUI(t *testing.T) {
	e := setup(t, func(s *Server, _ *fanout.Hub, _ *hubView) { s.UI = ui.FS })
	get := func(path string) (int, http.Header, string) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, e.ts.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		r, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Body.Close() }()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, r.Header, string(b)
	}
	for _, tc := range []struct{ path, ctype string }{
		{"/", "text/html"}, {"/r/" + roomID, "text/html"},
		{"/assets/app.js", "text/javascript"}, {"/assets/app.css", "text/css"},
	} {
		code, h, body := get(tc.path)
		if code != http.StatusOK || !strings.HasPrefix(h.Get("Content-Type"), tc.ctype) || body == "" {
			t.Fatalf("%s: %d %q", tc.path, code, h.Get("Content-Type"))
		}
		if h.Get("Content-Security-Policy") != csp || h.Get("X-Content-Type-Options") != "nosniff" ||
			h.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("%s: headers %v", tc.path, h)
		}
	}
	for _, d := range []string{"script-src 'self'", "connect-src 'self'", "frame-ancestors 'none'", "default-src 'none'"} {
		if !strings.Contains(csp, d) {
			t.Fatalf("csp lacks %q", d)
		}
	}
	_, _, index := get("/")
	if strings.Count(index, "<script") != 1 || !strings.Contains(index, `<script type="module" src="/assets/app.js"></script>`) {
		t.Fatalf("index.html must load only /assets/app.js, no inline script:\n%s", index)
	}
	// Nothing is fetched from anywhere but the broker.
	for _, f := range []string{"/", "/assets/app.js", "/assets/app.css"} {
		if _, _, body := get(f); strings.Contains(body, "src=\"http") || strings.Contains(body, "href=\"http") ||
			strings.Contains(body, "@import") {
			t.Fatalf("%s references another origin", f)
		}
	}
}

// Every response, API and refusal included, carries the hardening headers.
func TestEveryResponseIsHardened(t *testing.T) {
	e := setup(t)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, e.ts.URL+"/api/rooms", nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized || r.Header.Get("X-Content-Type-Options") != "nosniff" ||
		r.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("%d %v", r.StatusCode, r.Header)
	}
}

// The web client alone is the web UI; an unreadable client id makes nobody one.
func TestWebUI(t *testing.T) {
	for name, tc := range map[string]struct {
		web  string
		want bool
	}{"the web client": {"web", true}, "another client": {"roomctl", false}, "an unreadable client id": {"", false}} {
		t.Run(name, func(t *testing.T) {
			e := setup(t, func(s *Server, _ *fanout.Hub, _ *hubView) { s.WebClient = func() string { return tc.web } })
			c := dial(t, e, "dev", hello(nil, 0))
			if f := read(t, c); f.Snapshot.You.WebUI != tc.want {
				t.Fatalf("webUI %v", f.Snapshot.You.WebUI)
			}
		})
	}
	t.Run("a principal with no client id", func(t *testing.T) {
		e := setup(t, func(s *Server, _ *fanout.Hub, _ *hubView) {
			s.WebClient = func() string { return "" }
			s.Humans = authFunc(func(r *http.Request) (authn.Principal, error) {
				p, err := headerAuth{}.Authenticate(r)
				p.ClientID = ""
				return p, err
			})
		})
		c := dial(t, e, "dev", hello(nil, 0))
		if f := read(t, c); f.Snapshot.You.WebUI {
			t.Fatal("an empty client id matched an unreadable one")
		}
	})
}

type authFunc func(*http.Request) (authn.Principal, error)

func (f authFunc) Authenticate(r *http.Request) (authn.Principal, error) { return f(r) }

func TestSnapshotRuns(t *testing.T) {
	e := setup(t)
	runs := runwatch.New()
	e.srv.Runs = runs
	for _, r := range []struct{ id, room, role string }{{"7f3cq2xz", roomID, "implementer"}, {"2abcdefg", roomID, "reviewer"}, {"9zzzzzzz", "4kq7x2ma", "x"}} {
		runs.Upsert(t.Context(), agentRun(r.id, r.room, r.role))
	}
	c := dial(t, e, "dev", hello(nil, 0))
	f := read(t, c)
	b, _ := json.Marshal(f.Snapshot.Runs)
	if want := `[{"id":"2abcdefg","role":"reviewer","phase":"Running"},{"id":"7f3cq2xz","role":"implementer","phase":"Running"}]`; string(b) != want {
		t.Fatalf("runs %s", b)
	}
}

func agentRun(id, room, role string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "xplane-run-" + id, "namespace": runwatch.Namespace},
		"spec":     map[string]any{"roomRef": room, "role": role},
		"status":   map[string]any{"phase": "Running"},
	}}
}

// slotsFree reports whether no connection holds a slot.
func (s *Server) slotsFree() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.perUser) == 0 && len(s.perRoom) == 0
}

// released waits until the server holds no connection, slot or subscription.
func released(t *testing.T, e env) {
	t.Helper()
	eventually(t, "the handler ends", func() bool {
		return strings.Contains(scrape(t, e), `rooms_connections{kind="human"} 0`)
	})
	eventually(t, "the slot is freed", e.srv.slotsFree)
	eventually(t, "the subscription is released", func() bool { return e.subs.open.Load() == 0 })
}

// A peer whose TCP window stopped is cut within WriteWait and counted: the
// broker closed it, whatever the reader then saw (review I1, I2).
func TestAPeerThatStopsReadingIsCut(t *testing.T) {
	e := setup(t, func(s *Server, _ *fanout.Hub, _ *hubView) { s.WriteWait = 50 * time.Millisecond })
	e.log.add(80_000) // far past the loopback socket buffers
	c, _, err := connect(t, e, header("dev"))
	if err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	send(t, c, hello(&zero, 0)) // then never read
	eventually(t, "the drop is counted", func() bool {
		return strings.Contains(scrape(t, e), `rooms_connections_dropped_total{reason="write_timeout"} 1`)
	})
	released(t, e)
}

// The budget covers what the socket has not taken: a viewer that keeps reading
// moves many budgets' worth of events and is never dropped (review I2).
func TestAReadingViewerOutlastsItsBudget(t *testing.T) {
	e := setup(t, func(_ *Server, h *fanout.Hub, _ *hubView) { h.Budget = 1000 }) // about 22 events
	after := int64(10)
	c := dial(t, e, "dev", hello(&after, 0))
	read(t, c)
	read(t, c)
	for i := range 20 { // 200 events, about 9 budgets
		e.log.add(10)
		consecutive(t, events(t, c, 10), int64(11+10*i))
	}
}

// However a connection ends, its hub subscription goes with it (review I2).
func TestEverySubscriptionIsReleased(t *testing.T) {
	for name, tc := range map[string]struct {
		opt option
		end func(env, *websocket.Conn)
	}{
		"the peer leaves": {nil, func(_ env, c *websocket.Conn) { _ = c.Close(websocket.StatusNormalClosure, "") }},
		"a slow consumer": {func(_ *Server, h *fanout.Hub, _ *hubView) { h.Budget = 100 }, func(e env, _ *websocket.Conn) { e.log.add(50) }},
		"shutdown":        {nil, func(e env, _ *websocket.Conn) { e.srv.closeAll() }},
	} {
		t.Run(name, func(t *testing.T) {
			var opts []option
			if tc.opt != nil {
				opts = append(opts, tc.opt)
			}
			e := setup(t, opts...)
			c := dial(t, e, "dev", hello(nil, 0))
			read(t, c)
			read(t, c)
			if n := e.subs.open.Load(); n != 1 {
				t.Fatalf("%d subscriptions open, want 1", n)
			}
			tc.end(e, c)
			go func() { // read on, so the broker's close handshake completes
				for {
					if _, _, err := c.Read(context.Background()); err != nil {
						return
					}
				}
			}()
			released(t, e)
		})
	}
}

// MaxLifetime caps a connection whose token outlives it, closing 4001 (review M1).
func TestTheLifetimeCap(t *testing.T) {
	e := setup(t, func(s *Server, _ *fanout.Hub, _ *hubView) { s.MaxLifetime = 200 * time.Millisecond })
	c := dial(t, e, "dev", hello(nil, 0)) // the token lives 1 h
	if code, reason := closed(t, c); code != closeReauth || reason != "reauth" {
		t.Fatalf("closed %d %q", code, reason)
	}
}

// The mark is read after subscribing, so an append racing the subscription is
// replayed at once, not left as a hole until the next one (review M2).
func TestAnAppendRacingTheSubscriptionIsReplayed(t *testing.T) {
	for name, n := range map[string]int{"after the pre-upgrade read": 1, "after the second read": 2} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			e.log.mu.Lock()
			e.log.appendAfter = n
			e.log.mu.Unlock()
			after := int64(10)
			c := dial(t, e, "dev", hello(&after, 0))
			var seqs []int64
			for len(seqs) == 0 || seqs[len(seqs)-1] < 11 {
				if f := read(t, c); f.Type == wire.FrameEvent {
					seqs = append(seqs, f.Event.Seq)
				}
			}
			consecutive(t, seqs, 11)
		})
	}
}

// The snapshot's you is resolved against the driver the snapshot reports, read
// after subscribing, not against the pre-upgrade read (review M5).
func TestYouFollowsTheSnapshotsDriver(t *testing.T) {
	e := setup(t)
	e.log.mu.Lock()
	e.log.driverFrom, e.log.driver = 2, "human:dev"
	e.log.mu.Unlock()
	c := dial(t, e, "dev", hello(nil, 0))
	if f := read(t, c); f.Snapshot.Driver != "human:dev" || !f.Snapshot.You.Driver {
		t.Fatalf("driver %q, you %+v", f.Snapshot.Driver, f.Snapshot.You)
	}
}

// A Server missing a dependency refuses to serve rather than panic per request (review M6).
func TestServeRefusesAMissingDependency(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	e := setup(t)
	e.srv.WebClient = nil
	if err := e.srv.Serve(t.Context(), ln, time.Second); err == nil {
		t.Fatal("served without a WebClient")
	}
}
