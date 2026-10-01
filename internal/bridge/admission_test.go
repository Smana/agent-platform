// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"net/http"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/wire"
)

// F15: the gate holds the harness on Admission, so the first hello decides
// whether the run may start at all.
func TestAdmission(t *testing.T) {
	cases := []struct {
		name   string
		broker *fakeBroker
		want   Admission
	}{
		{"the first accepted hello admits the run", &fakeBroker{}, Admission{Admitted: true}},
		{"a sealed room refuses the run", &fakeBroker{hellos: []reply{{code: http.StatusGone, reason: wire.ReasonSealed}}},
			Admission{Refused: wire.ReasonSealed}},
		{"a busy room that frees within the patience admits the run",
			&fakeBroker{hellos: []reply{{code: http.StatusConflict, reason: wire.ReasonRoomBusy}}}, Admission{Admitted: true}},
		{"a broker outage only delays it",
			&fakeBroker{hellos: []reply{{code: http.StatusServiceUnavailable, reason: wire.ReasonLogUnavailable}}}, Admission{Admitted: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeAgentServer{pageSize: 100, status: "running"}
			r := newRig(t, NewHarness(f.start(t, conv).URL, conv), c.broker)
			if got := r.b.Admission(); got != (Admission{}) {
				t.Fatalf("Admission before any hello = %+v, want pending", got)
			}
			ctx, _ := r.run(t)
			eventually(ctx, t, "the hello is decided", func() bool { return r.b.Admission() != (Admission{}) })
			if got := r.b.Admission(); got != c.want {
				t.Fatalf("Admission = %+v, want %+v", got, c.want)
			}
		})
	}
}

// F15: a room another live run keeps past busyPatience refuses this run for
// good. Until then the run is only pending, so the harness waits rather than
// running unrecorded beside the holder.
func TestARoomKeptBusyPastThePatienceRefusesTheRun(t *testing.T) {
	f := &fakeAgentServer{pageSize: 100, status: "running"}
	fb := &fakeBroker{busy: true}
	r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
	clock := &fakeClock{t: time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)}
	r.b.Now = clock.now
	ctx, _ := r.run(t)

	eventually(ctx, t, "the broker answers room_busy twice", func() bool { return len(fb.callLog()) >= 2 })
	clock.add(busyPatience - time.Second)
	n := len(fb.callLog())
	eventually(ctx, t, "a hello just inside the patience", func() bool { return len(fb.callLog()) > n })
	if got := r.b.Admission(); got != (Admission{}) {
		t.Fatalf("Admission inside the patience = %+v, want pending", got)
	}

	clock.add(time.Second)
	eventually(ctx, t, "the run is refused", func() bool { return r.b.Admission() != (Admission{}) })
	if got, want := r.b.Admission(), (Admission{Refused: wire.ReasonRoomBusy}); got != want {
		t.Fatalf("Admission past the patience = %+v, want %+v", got, want)
	}
	calls := len(fb.callLog())
	time.Sleep(50 * time.Millisecond) // ten polls
	if got := fb.callLog(); len(got) != calls {
		t.Fatalf("a refused bridge kept saying hello: %v", got[calls:])
	}
	if f.searched() != 0 {
		t.Fatal("a refused bridge read the harness")
	}
	if !r.b.Healthy(time.Now()) {
		t.Fatal("a refused run made the bridge unhealthy: the kubelet would restart it for nothing")
	}
}
