// SPDX-License-Identifier: Apache-2.0

package rooms

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/bridgeapi"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/httpx"
	"github.com/Smana/agent-platform/internal/runwatch"
	"github.com/Smana/agent-platform/internal/store"
	"github.com/Smana/agent-platform/internal/wire"
)

// memLog is the part of the store the system API uses, in memory: append with the store's
// idempotency key (room, origin client, origin seq), range, and the room's state.
type memLog struct {
	mu     sync.Mutex
	events map[string][]envelope.Event
	keys   map[string]envelope.Event
}

func (l *memLog) Append(_ context.Context, d envelope.Draft) (envelope.Event, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	evs, ok := l.events[d.RoomID]
	if !ok {
		return envelope.Event{}, false, store.ErrNoRoom
	}
	key := fmt.Sprintf("%s/%s/%d", d.RoomID, d.OriginClient, d.OriginSeq)
	if e, dup := l.keys[key]; dup {
		return e, true, nil
	}
	e := envelope.Event{V: 1, Seq: int64(len(evs) + 1), RoomID: d.RoomID, Actor: d.Actor, Type: d.Type, Origin: d.Origin,
		TS: time.Now(), Redactions: []string{}, Payload: d.Payload}
	l.events[d.RoomID], l.keys[key] = append(evs, e), e
	return e, false, nil
}

func (l *memLog) AppendAsBridge(context.Context, string, envelope.Draft) (envelope.Event, bool, error) {
	return envelope.Event{}, false, errors.New("unused")
}

func (l *memLog) Range(_ context.Context, room string, after int64, limit int) ([]envelope.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []envelope.Event
	for _, e := range l.events[room] {
		if e.Seq > after && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (l *memLog) Deliveries(context.Context, string, string, int64, int64, int) ([]envelope.Event, error) {
	return nil, errors.New("unused")
}

func (l *memLog) Cursor(context.Context, string, string) (int64, error) { return 0, nil }

func (l *memLog) Room(_ context.Context, id string) (store.RoomState, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	evs, ok := l.events[id]
	if !ok {
		return store.RoomState{}, store.ErrNoRoom
	}
	return store.RoomState{ID: id, LastSeq: int64(len(evs))}, nil
}

func (l *memLog) ClaimBridge(context.Context, string, string, time.Duration, func(context.Context, string) bool) (string, bool, error) {
	return "", false, errors.New("unused")
}

func (l *memLog) TouchBridge(context.Context, string, string) (bool, error) {
	return false, errors.New("unused")
}

type noRedact struct{}

func (noRedact) Payload(_ context.Context, raw json.RawMessage) (json.RawMessage, []string, error) {
	return raw, nil, nil
}

// systems admits the bearer "factory-token" as system:factory, as authn.Systems does for an
// allowlisted sub.
type systems struct{}

func (systems) Authenticate(r *http.Request) (authn.Principal, error) {
	if r.Header.Get("Authorization") != "Bearer factory-token" {
		return authn.Principal{}, authn.ErrUnauthenticated
	}
	return authn.Principal{Kind: envelope.ActorSystem, ID: "system:factory", Sub: "sub", Expiry: time.Now().Add(time.Hour)}, nil
}

type noRuns struct{}

func (noRuns) Live(string) (runwatch.Run, bool) { return runwatch.Run{}, false }

// The client against the broker's own handler, not a copy of it: bridgeapi's routes, strict
// decoding, status codes and wire reasons are the contract.
func TestTheClientSpeaksTheBrokersSystemAPI(t *testing.T) {
	srv := &bridgeapi.Server{Log: &memLog{events: map[string][]envelope.Event{"3buqdlot": {}}, keys: map[string]envelope.Event{}},
		Redactor: noRedact{},
		Runs:     systems{}, Systems: systems{}, Watch: noRuns{}}
	ts := httptest.NewTLSServer(srv.Routes())
	defer ts.Close()
	roots := x509.NewCertPool()
	roots.AddCert(ts.Certificate())
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("factory-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(ts.URL, tok, httpx.New(5*time.Second, roots), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := c.TaskState(ctx, "3buqdlot", "Implementing", 1); err != nil {
		t.Fatalf("201: %v", err)
	}
	if err := c.TaskState(ctx, "3buqdlot", "Implementing", 1); err != nil {
		t.Fatalf("200 on a replayed clientSeq: %v", err)
	}
	if err := c.TaskState(ctx, "3buqdlot", "Reviewing", 2); err != nil {
		t.Fatal(err)
	}
	evs, last, err := c.Events(ctx, "3buqdlot", 0, maxLimit)
	if err != nil || last != 2 || len(evs) != 2 {
		t.Fatalf("%v %d %v", evs, last, err)
	}
	var p envelope.MessagePayload
	if err := json.Unmarshal(evs[1].Payload, &p); err != nil || p.Kind != envelope.KindTaskState || p.Text != "Reviewing" ||
		evs[1].Actor.ID != "system:factory" {
		t.Fatalf("%+v %+v %v", evs[1], p, err)
	}
	all, cursor, err := c.EventsSince(ctx, "3buqdlot", 1)
	if err != nil || len(all) != 1 || cursor != 2 {
		t.Fatalf("%v %d %v", all, cursor, err)
	}
	var api *APIError
	if _, _, err := c.Events(ctx, "aaaaaaaa", 0, 10); !errors.As(err, &api) || api.Status != http.StatusNotFound || api.Reason != wire.ReasonNoRoom {
		t.Fatalf("a room the log lacks: %v", err)
	}
	if err := os.WriteFile(tok, []byte("stolen\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.TaskState(ctx, "3buqdlot", "x", 3); !errors.As(err, &api) || api.Status != http.StatusUnauthorized ||
		api.Reason != wire.ReasonUnauthenticated {
		t.Fatalf("a refused token: %v", err)
	}
}
