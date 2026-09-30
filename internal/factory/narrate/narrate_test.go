// SPDX-License-Identifier: Apache-2.0

package narrate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/rooms"
	"github.com/Smana/agent-platform/internal/factory/runs"
	"github.com/Smana/agent-platform/internal/wire"
)

const bot = "ogenki-agent-factory[bot]" // forge.Fake's author, config.factoryLogin

func task() *v1alpha1.Task {
	return &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "3buqdlot"},
		Spec:   v1alpha1.TaskSpec{Issue: 7, Budget: v1alpha1.Budget{Tier: "standard"}},
		Status: v1alpha1.TaskStatus{RoomRef: "3buqdlot"}}
}

// Δ6: started carries the run id, the branch, the budget and a watch link.
func TestStartedCarriesWhatAHumanNeeds(t *testing.T) {
	e := Started(task(), runs.Spec{RunID: "7f3cq2xz", Role: "implementer", Branch: "agent/3buqdlot",
		MaxTokens: 1_500_000, MaxMinutes: 45}, "https://rooms.priv.aws.ogenki.io/")
	for _, want := range []string{"7f3cq2xz", "agent/3buqdlot", "1.5 M tokens", "45 minutes", "tier standard",
		"https://rooms.priv.aws.ogenki.io/r/3buqdlot", "factory/stop"} {
		if !strings.Contains(e.Body, want) {
			t.Errorf("started lacks %q:\n%s", want, e.Body)
		}
	}
	if e.Key != "run-7f3cq2xz-started" {
		t.Errorf("key %q", e.Key)
	}
}

func TestEndedExplainsTheReason(t *testing.T) {
	tk := task()
	tk.Status.Usage.Tokens = 1_600_000
	e := Ended(tk, v1alpha1.PhaseEscalated, "budget-run")
	if !strings.Contains(e.Body, "needs a maintainer: the run spent its token budget.") || !strings.Contains(e.Body, "1.6 M") ||
		!strings.Contains(e.Body, "Re-apply `factory/ready`") {
		t.Fatal(e.Body)
	}
	if Ended(tk, v1alpha1.PhaseEscalated, "pod_lost").Key == Ended(tk, v1alpha1.PhaseDone, "merged").Key {
		t.Fatal("each ending has its own key")
	}
	tk.Status.PullRequest = &v1alpha1.PullRequestRef{Number: 12, MergedBy: "Smana"}
	if d := Ended(tk, v1alpha1.PhaseDone, "merged"); !strings.Contains(d.Body, "Merged by @Smana.") {
		t.Fatal(d.Body)
	}
	if r := Ended(tk, v1alpha1.PhaseReverted, "main_red"); strings.Contains(r.Body, "Merged by") {
		t.Fatalf("only a done task credits its merger: %s", r.Body)
	}
}

func TestPROpenedAndRefused(t *testing.T) {
	if e := PROpened(task(), 12, "https://github.com/Smana/cloud-native-ref/pull/12", "7f3cq2xz"); e.Key != "pr-opened" ||
		!strings.Contains(e.Body, "#12") || !strings.Contains(e.Body, "7f3cq2xz") {
		t.Fatalf("%+v", e)
	}
	at := time.Unix(1_790_000_000, 0)
	e := Refused(7, "edited_after_label", at)
	if e.Key != "refused-edited_after_label-1790000000" || !strings.Contains(e.Body, "edited after it was labelled") {
		t.Fatalf("%+v", e)
	}
	if Refused(7, "edited_after_label", at.Add(time.Second)).Key == e.Key {
		t.Fatal("each label is answered once: the key carries its time")
	}
}

func TestTokensAndReasons(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1_000: "1 k", 300_000: "300 k", 1_000_000: "1.0 M", 1_550_000: "1.6 M"} {
		if got := Tokens(n); got != want {
			t.Errorf("Tokens(%d) = %q, want %q", n, got, want)
		}
	}
	if Reason("kill_switch") != "the factory's kill switch is engaged" || Reason("something_new") != "something_new" {
		t.Fatal("known reasons read as prose; an unknown one is shown as it is")
	}
}

// R22: one comment per event, even across a crash between posting and recording.
func TestPostIsIdempotent(t *testing.T) {
	f := forge.NewFake()
	n := Narrator{Forge: f, Login: bot}
	tk := task()
	e := Event{Key: "pr-opened", Body: "Run 7f3cq2xz opened #12."}
	for range 2 {
		if err := n.Post(t.Context(), tk, 7, e); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(tk.Status.Narrated, []string{"pr-opened"}) {
		t.Fatalf("a key is recorded once: %q", tk.Status.Narrated)
	}
	tk.Status.Narrated = nil // the status write was lost
	if err := n.Post(t.Context(), tk, 7, e); err != nil {
		t.Fatal(err)
	}
	if got := f.Comments(7); len(got) != 1 || !strings.Contains(got[0], Marker("3buqdlot", "pr-opened")) {
		t.Fatalf("%q", got)
	}
	if !slices.Equal(tk.Status.Narrated, []string{"pr-opened"}) {
		t.Fatalf("the marker found is recorded: %q", tk.Status.Narrated)
	}
	if err := n.Post(t.Context(), tk, 0, Event{Key: "ended", Body: "x"}); err != nil || len(f.Comments(0)) != 0 {
		t.Fatal("number 0 means nowhere to narrate")
	}
	// The factory's own comment carrying another event's marker is no dedup for this one.
	if err := n.Post(t.Context(), tk, 7, Event{Key: "ended", Body: "Done."}); err != nil || len(f.Comments(7)) != 2 {
		t.Fatalf("%v %q", err, f.Comments(7))
	}
}

// Anyone can comment on a public issue: a marker only counts in the factory's own comments, so
// nobody can silence a narration by posting it first.
func TestAMarkerCountsOnlyInTheFactorysComments(t *testing.T) {
	f := &scripted{Fake: forge.NewFake(), recent: []forge.Comment{
		{Author: "someone", Body: "hi " + Marker("3buqdlot", "pr-opened")},
		{Author: "ogenki-agent-factory", Body: Marker("3buqdlot", "pr-opened")}, // a user, not the App
	}}
	n := Narrator{Forge: f, Login: bot}
	if err := n.PostOnce(t.Context(), 7, "3buqdlot", Event{Key: "pr-opened", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	if len(f.Comments(7)) != 1 {
		t.Fatal("a forged marker silenced the narration")
	}
	f.recent = append(f.recent, forge.Comment{Author: bot, Body: "x\n\n" + Marker("3buqdlot", "pr-opened")})
	if err := n.PostOnce(t.Context(), 7, "3buqdlot", Event{Key: "pr-opened", Body: "x"}); err != nil || len(f.Comments(7)) != 1 {
		t.Fatal("the factory's own marker is the dedup")
	}
}

func TestAFailedCallRecordsNothing(t *testing.T) {
	for name, f := range map[string]*scripted{
		"listing fails": {Fake: forge.NewFake(), listErr: errors.New("502")},
		"posting fails": {Fake: forge.NewFake(), postErr: errors.New("502")},
	} {
		t.Run(name, func(t *testing.T) {
			tk := task()
			err := Narrator{Forge: f, Login: bot}.Post(t.Context(), tk, 7, Event{Key: "pr-opened", Body: "x"})
			if err == nil || len(tk.Status.Narrated) != 0 {
				t.Fatalf("%v %q", err, tk.Status.Narrated)
			}
		})
	}
}

func TestNarratorNeedsItsLogin(t *testing.T) {
	f := forge.NewFake()
	if err := (Narrator{Forge: f}).PostOnce(t.Context(), 7, "3buqdlot", Event{Key: "k", Body: "x"}); err == nil || len(f.Comments(7)) != 0 {
		t.Fatal("without the factory's login no marker can be trusted")
	}
}

// The CRD holds 512 keys (review I3): the oldest go, the newest stay.
func TestNarratedStaysBounded(t *testing.T) {
	f := forge.NewFake()
	n := Narrator{Forge: f, Login: bot}
	tk := task()
	for i := range MaxNarrated + 40 {
		if err := n.Post(t.Context(), tk, 7, Event{Key: fmt.Sprintf("k%d", i), Body: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if l := len(tk.Status.Narrated); l != MaxNarrated || tk.Status.Narrated[l-1] != fmt.Sprintf("k%d", MaxNarrated+39) ||
		slices.Contains(tk.Status.Narrated, "k39") || !slices.Contains(tk.Status.Narrated, "k40") {
		t.Fatalf("%d keys, first %q", l, tk.Status.Narrated[0])
	}
}

type scripted struct {
	*forge.Fake
	recent           []forge.Comment
	listErr, postErr error
}

func (s *scripted) RecentComments(context.Context, int) ([]forge.Comment, error) {
	return s.recent, s.listErr
}

func (s *scripted) Comment(ctx context.Context, n int, body string) error {
	if s.postErr != nil {
		return s.postErr
	}
	return s.Fake.Comment(ctx, n, body)
}

// poster is the broker as the room sees it: it dedups on clientSeq alone, as bridgeapi does.
type poster struct {
	calls []string // "persist <narrated>" and "post <seq>", in order
	seen  map[int64]string
	err   error
}

func (p *poster) TaskState(_ context.Context, room, text string, clientSeq int64) error {
	p.calls = append(p.calls, fmt.Sprintf("post %s %d", room, clientSeq))
	if p.err != nil {
		return p.err
	}
	if p.seen == nil {
		p.seen = map[int64]string{}
	}
	if _, ok := p.seen[clientSeq]; !ok {
		p.seen[clientSeq] = text
	}
	return nil
}

func (p *poster) persist(t *v1alpha1.Task) func(context.Context) error {
	return func(context.Context) error {
		p.calls = append(p.calls, fmt.Sprintf("persist %d %s", t.Status.RoomSeq, t.Status.Narrated[len(t.Status.Narrated)-1]))
		return nil
	}
}

// Ruling SK: the clientSeq is status.roomSeq + 1, persisted before the post, so a restart replays
// the same seq and the broker keeps one message.
func TestRoomClientSeqIsRoomSeqPlusOnePersistedFirst(t *testing.T) {
	p := &poster{}
	tk := task()
	tk.Status.RoomSeq = 2
	tk.Status.Narrated = []string{"run-7f3cq2xz-started", "pr-opened"}
	if err := Room(t.Context(), p, tk, "snapshot", "the snapshot", p.persist(tk)); err != nil {
		t.Fatal(err)
	}
	want := []string{"persist 3 room/3/snapshot", "post 3buqdlot 3"}
	if !slices.Equal(p.calls, want) || tk.Status.RoomSeq != 3 {
		t.Fatalf("%q roomSeq %d", p.calls, tk.Status.RoomSeq)
	}
	if err := Room(t.Context(), p, tk, "round-1", "round 1", p.persist(tk)); err != nil {
		t.Fatal(err)
	}
	want = append(want, "persist 4 room/4/round-1", "post 3buqdlot 4")
	if !slices.Equal(p.calls, want) || len(p.seen) != 2 {
		t.Fatalf("%q %v", p.calls, p.seen)
	}
}

// A restart between the post and whatever the caller records next replays the post: the seq it
// sends is the one it sent, unchanged, so the broker answers 200 and stores nothing new.
func TestARestartReplaysTheSameClientSeq(t *testing.T) {
	p := &poster{err: errors.New("connection reset")}
	tk := task()
	if err := Room(t.Context(), p, tk, "snapshot", "the snapshot", p.persist(tk)); err == nil {
		t.Fatal("the post failed")
	}
	p.err = nil
	restarted := tk.DeepCopy() // what the persisted status holds
	for range 2 {
		if err := Room(t.Context(), p, restarted, "snapshot", "the snapshot", p.persist(restarted)); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"persist 1 room/1/snapshot", "post 3buqdlot 1", "post 3buqdlot 1", "post 3buqdlot 1"}
	if !slices.Equal(p.calls, want) || restarted.Status.RoomSeq != 1 || len(p.seen) != 1 {
		t.Fatalf("%q roomSeq %d", p.calls, restarted.Status.RoomSeq)
	}
}

// The review's B2: narrated is capped at 512 and trimmed, so the seq cannot come from it. Past the
// cap, every room message still gets its own, increasing clientSeq.
func TestEveryClientSeqIsDistinctAndIncreasingPastTheCap(t *testing.T) {
	p := &poster{}
	n := Narrator{Forge: forge.NewFake(), Login: bot}
	tk := task()
	for i := range MaxNarrated + 88 {
		if err := n.Post(t.Context(), tk, 7, Event{Key: fmt.Sprintf("k%d", i), Body: "x"}); err != nil {
			t.Fatal(err)
		}
		if err := Room(t.Context(), p, tk, fmt.Sprintf("r%d", i), "m", p.persist(tk)); err != nil {
			t.Fatal(err)
		}
	}
	var last int64
	for _, c := range p.calls {
		var seq int64
		if _, err := fmt.Sscanf(c, "post 3buqdlot %d", &seq); err != nil {
			continue
		}
		if seq <= last {
			t.Fatalf("clientSeq %d after %d", seq, last)
		}
		last = seq
	}
	if last != MaxNarrated+88 || len(p.seen) != MaxNarrated+88 || len(tk.Status.Narrated) > MaxNarrated {
		t.Fatalf("last %d, %d stored, %d keys", last, len(p.seen), len(tk.Status.Narrated))
	}
}

func TestRoomSendsNothingItCouldNotRecord(t *testing.T) {
	p := &poster{}
	tk := task()
	err := Room(t.Context(), p, tk, "snapshot", "s", func(context.Context) error { return errors.New("conflict") })
	if err == nil || len(p.calls) != 0 || len(tk.Status.Narrated) != 0 || tk.Status.RoomSeq != 0 {
		t.Fatalf("%v %q %q %d", err, p.calls, tk.Status.Narrated, tk.Status.RoomSeq)
	}
	if err := Room(t.Context(), p, tk, "snapshot", "s", nil); err == nil || len(p.calls) != 0 {
		t.Fatal("no persist, no seq")
	}
	tk.Status.RoomRef = ""
	if err := Room(t.Context(), p, tk, "snapshot", "s", p.persist(tk)); err == nil || len(p.calls) != 0 {
		t.Fatal("a task without a room has nowhere to post")
	}
}

// The broker's refusals stay typed: no_room is a retry, and not_permitted is expected until FR-1
// enables the factory's systemPrincipals entry.
func TestRoomRefusalsStayTyped(t *testing.T) {
	for _, c := range []struct {
		status int
		reason string
		is     error
	}{
		{http.StatusNotFound, wire.ReasonNoRoom, rooms.ErrNoRoom},
		{http.StatusForbidden, wire.ReasonNotPermitted, rooms.ErrNotPermitted},
	} {
		p := &poster{err: &rooms.APIError{Status: c.status, Reason: c.reason}}
		tk := task()
		err := Room(t.Context(), p, tk, "snapshot", "s", p.persist(tk))
		if !errors.Is(err, c.is) {
			t.Errorf("%s: %v", c.reason, err)
		}
		if tk.Status.RoomSeq != 1 || !slices.Contains(tk.Status.Narrated, "room/1/snapshot") {
			t.Errorf("%s: the seq stays recorded, so the retry reuses it", c.reason)
		}
	}
}
