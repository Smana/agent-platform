// SPDX-License-Identifier: Apache-2.0

package runwatch

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Smana/agent-platform/internal/envelope"
)

type fakeUnfinished struct {
	evs    []envelope.Event
	err    error
	prefix string
	first  int64
	last   int64
}

func (f *fakeUnfinished) Unfinished(_ context.Context, prefix string, first, last int64) ([]envelope.Event, error) {
	f.prefix, f.first, f.last = prefix, first, last
	return f.evs, f.err
}

func joined(roomID, runID, role string) envelope.Event {
	return envelope.Event{RoomID: roomID, RunID: runID, Type: envelope.Participant,
		Payload: envelope.Must(envelope.ParticipantPayload{Principal: "agent:" + runID, Change: "joined", Role: role})}
}

func TestSweep(t *testing.T) {
	running := Run{ID: "7f3cq2xz", Room: "3kq7x2ma", Role: "implementer", Phase: "Running"}
	for _, c := range []struct {
		name  string
		open  []envelope.Event
		known map[string]Run
		want  []string // the drafts appended, as type:payload
	}{
		{"a joined run whose claim is gone ends as deleted, with its role",
			[]envelope.Event{joined("3kq7x2ma", "7f3cq2xz", "reviewer")}, nil,
			[]string{
				`state_changed:{"kind":"run_phase","phase":"Running"}`,
				`state_changed:{"kind":"run_phase","phase":"Revoked","reason":"deleted"}`,
				`participant:{"principal":"agent:7f3cq2xz","change":"left","role":"reviewer"}`,
			}},
		{"a run still watched is observed again, which retries a failed append",
			[]envelope.Event{joined("3kq7x2ma", "7f3cq2xz", "implementer")}, map[string]Run{running.ID: running},
			[]string{`state_changed:{"kind":"run_phase","phase":"Running"}`}},
		{"nothing unfinished appends nothing", nil, nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := t.Context()
			fs := &fakeStore{keys: map[string]bool{
				"broker:run:7f3cq2xz/1": true, // the joined event the sweep found
			}}
			src := &fakeUnfinished{evs: c.open}
			e := &Events{Store: fs}
			get := func(id string) (Run, bool) { r, ok := c.known[id]; return r, ok }
			if err := e.Sweep(ctx, src, get); err != nil {
				t.Fatal(err)
			}
			if src.prefix != "broker:run:" || src.first != stepJoined || src.last != stepLeft {
				t.Fatalf("asked for %q %d..%d", src.prefix, src.first, src.last)
			}
			if got := payloads(fs.drafts); !slices.Equal(got, c.want) {
				t.Fatalf("got %v\nwant %v", got, c.want)
			}
		})
	}
}

func TestSweepErrors(t *testing.T) {
	boom := errors.New("boom")
	for _, c := range []struct {
		name string
		src  *fakeUnfinished
		fs   *fakeStore
	}{
		{"the query fails", &fakeUnfinished{err: boom}, &fakeStore{keys: map[string]bool{}}},
		{"an append fails, and the others are still tried", &fakeUnfinished{evs: []envelope.Event{
			joined("3kq7x2ma", "7f3cq2xz", "implementer"), joined("3kq7x2ma", "aaaaaaaa", "implementer"),
		}}, &fakeStore{keys: map[string]bool{}, appendErr: boom}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := &Events{Store: c.fs}
			err := e.Sweep(t.Context(), c.src, func(string) (Run, bool) { return Run{}, false })
			if !errors.Is(err, boom) {
				t.Fatalf("got %v", err)
			}
		})
	}
}
