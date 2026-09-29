// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Smana/agent-platform/internal/bridge"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/metrics"
	"github.com/Smana/agent-platform/internal/store"
)

// appends are the store's two writes of an event.
type appends interface {
	Append(ctx context.Context, d envelope.Draft) (envelope.Event, bool, error)
	AppendAsBridge(ctx context.Context, bridgeRun string, d envelope.Draft) (envelope.Event, bool, error)
}

// meteredLog is the store every writer shares, its appends counted: the bridge
// API, the Room controller and the run lifecycle alike, so
// rooms_events_appended_total and rooms_redactions_total see every event, not
// only the API's. Every other method is the embedded store's.
type meteredLog struct {
	*store.Store
	appends appends // the same store in production; a fake in tests
	m       *metrics.Set
	now     func() time.Time
}

// Append is the store's, counted.
func (l *meteredLog) Append(ctx context.Context, d envelope.Draft) (envelope.Event, bool, error) {
	return l.count(ctx, d, func() (envelope.Event, bool, error) { return l.appends.Append(ctx, d) })
}

// AppendAsBridge is the store's, counted.
func (l *meteredLog) AppendAsBridge(ctx context.Context, bridgeRun string, d envelope.Draft) (envelope.Event, bool, error) {
	return l.count(ctx, d, func() (envelope.Event, bool, error) { return l.appends.AppendAsBridge(ctx, bridgeRun, d) })
}

func (l *meteredLog) count(ctx context.Context, d envelope.Draft, f func() (envelope.Event, bool, error)) (envelope.Event, bool, error) {
	start := l.now()
	ev, dup, err := f()
	l.m.AppendTook(ctx, l.now().Sub(start))
	switch {
	case err == nil && !dup:
		l.m.Appended(ctx, string(ev.Type), string(ev.Origin), ev.Redactions)
		l.bridgeSignals(ctx, ev)
	case err != nil && databaseFailed(d, err):
		l.m.AppendFailed(ctx)
	}
	return ev, dup, err
}

// maxSignalBytes skips decoding anything larger: stall notices and stubs are a
// few fixed fields, each cut to 64 bytes by the bridge.
const maxSignalBytes = 512

// isStall and isStub bound the counters' labels: a harness's own error codes and
// event kinds are its text, not ours.
func isStall(code string) bool {
	switch code {
	case bridge.StallEventTooLarge, bridge.StallCursorLost, bridge.StallNextPageUnreadable:
		return true
	}
	return false
}

func isStub(kind string) bool { return kind == bridge.StubOversize || kind == bridge.StubRefused }

// bridgeSignals counts what a bridge tells its room of its own trouble (Ruling
// AP): a stall on the harness log, told once per stall as state_changed
// {harness_error, code}, and an item kept as a stub, either the bridge's
// harness_event{harnessKind} or a size or refusal stub in the item's own type.
func (l *meteredLog) bridgeSignals(ctx context.Context, ev envelope.Event) {
	if ev.Origin != envelope.OriginHarness || len(ev.Payload) > maxSignalBytes {
		return
	}
	var p struct {
		Kind        string `json:"kind"`
		Code        string `json:"code"`
		HarnessKind string `json:"harnessKind"`
		Oversize    bool   `json:"oversize"`
		Refused     bool   `json:"refused"`
	}
	if json.Unmarshal(ev.Payload, &p) != nil {
		return
	}
	switch {
	case ev.Type == envelope.StateChanged && p.Kind == "harness_error" && isStall(p.Code):
		l.m.BridgeStalled(ctx, p.Code)
	case ev.Type == envelope.StateChanged && p.Kind == "harness_event" && isStub(p.HarnessKind):
		l.m.BridgeStubbed(ctx, p.HarnessKind)
	case p.Oversize: // envelope.Oversize, from the bridge or the store
		l.m.BridgeStubbed(ctx, bridge.StubOversize)
	case p.Refused: // the broker's own stub for a value it cannot store
		l.m.BridgeStubbed(ctx, bridge.StubRefused)
	}
}

// databaseFailed tells a failing database (RoomLogAppendErrors: check CNPG) from
// an append refused for what it is: an invalid draft, a missing or sealed room,
// a lost lease, a value PostgreSQL refuses, or a caller that went away.
func databaseFailed(d envelope.Draft, err error) bool {
	switch {
	case d.Validate() != nil, store.IsDataError(err), errors.Is(err, context.Canceled),
		errors.Is(err, store.ErrNoRoom), errors.Is(err, store.ErrSealed), errors.Is(err, store.ErrLeaseLost):
		return false
	}
	return true
}
