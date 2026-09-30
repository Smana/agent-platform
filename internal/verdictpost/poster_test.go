// SPDX-License-Identifier: Apache-2.0

package verdictpost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/github"
	"github.com/Smana/agent-platform/internal/store"
)

type fakeLog struct {
	verdicts  []envelope.Event
	expired   []envelope.Event
	records   []envelope.Draft
	appendErr error
	since     time.Time
	before    time.Time
	limit     int
	excluded  []string
}

func (f *fakeLog) recorded(v envelope.Event) bool {
	for _, r := range f.records {
		if r.RoomID == v.RoomID && r.OriginSeq == v.Seq {
			return true
		}
	}
	return false
}

func (f *fakeLog) UnpostedVerdicts(_ context.Context, since time.Time, limit int, exclude []string) ([]envelope.Event, error) {
	f.since, f.limit, f.excluded = since, limit, exclude
	var out []envelope.Event
	for _, v := range f.verdicts {
		if !f.recorded(v) && !slices.Contains(exclude, v.ID) && len(out) < limit {
			out = append(out, v)
		}
	}
	return out, nil
}

func (f *fakeLog) ExpiredVerdicts(_ context.Context, before time.Time, limit int) ([]envelope.Event, error) {
	f.before = before
	var out []envelope.Event
	for _, v := range f.expired {
		if !f.recorded(v) && len(out) < limit {
			out = append(out, v)
		}
	}
	return out, nil
}

func (f *fakeLog) Append(_ context.Context, d envelope.Draft) (envelope.Event, bool, error) {
	if f.appendErr != nil {
		return envelope.Event{}, false, f.appendErr
	}
	f.records = append(f.records, d)
	return envelope.Event{RoomID: d.RoomID, Seq: int64(100 + len(f.records))}, false, nil
}

type fakeGitHub struct {
	enabled bool
	err     error
	calls   []string
	onCall  func()
	hang    bool // block until the post's ctx ends
}

func (g *fakeGitHub) Enabled() bool { return g.enabled }

func (g *fakeGitHub) Comment(ctx context.Context, pr, marker, body string) (string, error) {
	g.calls = append(g.calls, pr+" "+marker+"\n"+body)
	if g.hang {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if g.onCall != nil {
		g.onCall()
	}
	if g.err != nil {
		return "", g.err
	}
	return pr + "#issuecomment-1", nil
}

const pr = "https://github.com/Smana/cloud-native-ref/pull/12"

func verdict(seq int64, pr string) envelope.Event {
	return envelope.Event{ID: fmt.Sprintf("v%d", seq), RoomID: "3kq7x2ma", Seq: seq, RunID: "7f3cq2xz", Type: envelope.Message,
		TS:    start,
		Actor: envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:7f3cq2xz", Role: "reviewer"},
		Payload: envelope.Must(envelope.MessagePayload{Kind: envelope.KindReviewVerdict, Text: "Missing a test.\ncc @Smana",
			Verdict: "changes", Commit: "4be1c9d", Delivery: envelope.DeliveryNone, PullRequest: pr})}
}

// start is when every test verdict was written, and the test clock's origin.
var start = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func poster(log *fakeLog, gh *fakeGitHub, results *[]string) (*Poster, *clock) {
	c := &clock{t: start}
	return &Poster{Log: log, GitHub: gh, PublicURL: "https://rooms.priv.aws.ogenki.io", Now: c.now,
		Leading:   func() bool { return true },
		DataClass: func(context.Context, string) string { return "public" },
		OnResult:  func(_ context.Context, r string) { *results = append(*results, r) }}, c
}

func record(t *testing.T, d envelope.Draft) map[string]any {
	t.Helper()
	if d.Type != envelope.StateChanged || d.Origin != envelope.OriginBroker || d.OriginClient != store.VerdictsClient ||
		d.Actor.Kind != envelope.ActorSystem || d.CausedBy == nil || *d.CausedBy != d.OriginSeq {
		t.Fatalf("record draft = %+v", d)
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("the store would refuse the record: %v", err)
	}
	var p map[string]any
	if err := json.Unmarshal(d.Payload, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPostsOnceAndRecordsIt(t *testing.T) {
	log, gh, results := &fakeLog{verdicts: []envelope.Event{verdict(42, pr)}}, &fakeGitHub{enabled: true}, []string{}
	p, c := poster(log, gh, &results)
	for range 2 {
		if err := p.Once(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(gh.calls) != 1 || !strings.HasPrefix(gh.calls[0], pr+" <!-- agent-room:3kq7x2ma:42 -->") {
		t.Fatalf("calls = %q", gh.calls)
	}
	if len(log.records) != 1 || log.records[0].OriginSeq != 42 || log.records[0].RoomID != "3kq7x2ma" {
		t.Fatalf("records = %+v", log.records)
	}
	if rec := record(t, log.records[0]); rec["kind"] != "verdict_posted" || rec["url"] != pr+"#issuecomment-1" || rec["verdictSeq"] != float64(42) {
		t.Fatalf("record = %v", rec)
	}
	if strings.Join(results, ",") != "posted" {
		t.Fatalf("results = %v", results)
	}
	if !log.since.Equal(c.t.Add(-Window)) || log.limit != batch {
		t.Fatalf("since %v limit %d", log.since, log.limit)
	}
}

func TestWaitsForTheApp(t *testing.T) {
	log, gh, results := &fakeLog{verdicts: []envelope.Event{verdict(42, pr)}}, &fakeGitHub{}, []string{}
	p, _ := poster(log, gh, &results)
	_ = p.Once(t.Context())
	if len(gh.calls) != 0 || len(log.records) != 0 {
		t.Fatal("no App key yet: the verdict waits, unrecorded")
	}
	gh.enabled = true
	_ = p.Once(t.Context())
	if len(gh.calls) != 1 {
		t.Fatal("posted once the key landed")
	}
}

func TestWhatCannotBePostedIsRecordedOnce(t *testing.T) {
	for _, tc := range []struct {
		name, reason, detail string
		pr                   string
		err                  error
		calls                int
	}{
		{"no pull request", "no_pull_request", "", "", nil, 0},
		{"GitHub says it is not a pull request", "no_pull_request", "", pr, fmt.Errorf("x: %w", github.ErrNotAPullRequest), 1},
		{"the App is not installed", "github_refused", "http_404", pr, &github.APIError{Status: 404, Path: "repos/Smana/cloud-native-ref/installation"}, 1},
		{"too long to search", "github_refused", "too_many_comments", pr, github.ErrTooManyComments, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, results := &fakeLog{verdicts: []envelope.Event{verdict(42, tc.pr)}}, []string{}
			gh := &fakeGitHub{enabled: true, err: tc.err}
			p, _ := poster(log, gh, &results)
			_ = p.Once(t.Context())
			_ = p.Once(t.Context())
			if len(log.records) != 1 || len(gh.calls) != tc.calls {
				t.Fatalf("%d records, %d calls", len(log.records), len(gh.calls))
			}
			rec := record(t, log.records[0])
			if rec["kind"] != "verdict_not_posted" || rec["reason"] != tc.reason || (rec["detail"] != nil && rec["detail"] != tc.detail) ||
				(tc.detail != "" && rec["detail"] != tc.detail) {
				t.Fatalf("record = %v", rec)
			}
			if strings.Join(results, ",") != "not_posted" {
				t.Fatalf("results = %v", results)
			}
		})
	}
}

func TestATransientErrorIsRetried(t *testing.T) {
	for name, err := range map[string]error{
		"GitHub down":      &github.APIError{Status: 502},
		"a network error":  errors.New("dial tcp: connection refused"),
		"no key file":      errors.New("github: private key: open /etc/x: permission denied"),
		"a revoked token":  nil,                           // Comment retried the 401 itself; a success here
		"a rejected key":   &github.APIError{Status: 401}, // ruling SZ: the operator can fix it
		"a rate limit 403": &github.APIError{Status: 403, RateLimited: true},
	} {
		t.Run(name, func(t *testing.T) {
			log, results := &fakeLog{verdicts: []envelope.Event{verdict(42, pr)}}, []string{}
			gh := &fakeGitHub{enabled: true, err: err}
			p, c := poster(log, gh, &results)
			_ = p.Once(t.Context())
			gh.err = nil
			c.t = c.t.Add(firstBackoff)
			_ = p.Once(t.Context())
			want := "error,posted"
			if err == nil {
				want = "posted"
			}
			if len(log.records) != 1 || strings.Join(results, ",") != want {
				t.Fatalf("calls %d, records %d, results %v", len(gh.calls), len(log.records), results)
			}
		})
	}
}

// GitHub's rate limit covers every comment: a Retry-After stops this tick and
// holds the poster until it lifts.
func TestARateLimitPausesThePoster(t *testing.T) {
	log, results := &fakeLog{verdicts: []envelope.Event{verdict(42, pr), verdict(43, pr)}}, []string{}
	gh := &fakeGitHub{enabled: true, err: &github.APIError{Status: 403, RateLimited: true, RetryAfter: time.Minute}}
	p, c := poster(log, gh, &results)
	_ = p.Once(t.Context())
	if len(gh.calls) != 1 {
		t.Fatalf("%d calls: the tick stops at the rate limit", len(gh.calls))
	}
	gh.err = nil
	c.t = c.t.Add(59 * time.Second)
	_ = p.Once(t.Context())
	if len(gh.calls) != 1 {
		t.Fatal("posted before the limit lifted")
	}
	c.t = c.t.Add(time.Second)
	_ = p.Once(t.Context())
	if len(gh.calls) != 3 || len(log.records) != 2 {
		t.Fatalf("%d calls, %d records", len(gh.calls), len(log.records))
	}
}

// Ruling SY: only the lease holder posts, checked before every post.
func TestPostsOnlyWhileLeading(t *testing.T) {
	log, results := &fakeLog{verdicts: []envelope.Event{verdict(42, pr), verdict(43, pr)}}, []string{}
	leading := true
	gh := &fakeGitHub{enabled: true, onCall: func() { leading = false }}
	p, _ := poster(log, gh, &results)
	p.Leading = func() bool { return leading }
	_ = p.Once(t.Context())
	if len(gh.calls) != 1 {
		t.Fatalf("%d calls after the lease was lost", len(gh.calls))
	}
	p.Leading = nil
	leading = true
	_ = p.Once(t.Context())
	if len(gh.calls) != 1 {
		t.Fatal("no lease check means no post")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p.Leading = func() bool { return true }
	_ = p.Once(ctx)
	if len(gh.calls) != 1 {
		t.Fatal("an ended lease context posts nothing")
	}
}

// A record that cannot be appended is an error: the verdict comes back next
// tick, and the App's marked comment makes the repeat a no-op.
func TestARecordThatFailsIsAnError(t *testing.T) {
	log, results := &fakeLog{verdicts: []envelope.Event{verdict(42, pr)}, appendErr: errors.New("store down")}, []string{}
	p, _ := poster(log, &fakeGitHub{enabled: true}, &results)
	_ = p.Once(t.Context())
	if strings.Join(results, ",") != "error" {
		t.Fatalf("results = %v", results)
	}
}

func TestBody(t *testing.T) {
	ev := verdict(42, pr)
	var v envelope.MessagePayload
	_ = json.Unmarshal(ev.Payload, &v)
	public := Body(ev, v, "public", "https://rooms.priv.aws.ogenki.io")
	for _, want := range []string{"### Agent review: changes requested", "```\nMissing a test.\ncc @Smana\n```\n",
		"[3kq7x2ma](https://rooms.priv.aws.ogenki.io/r/3kq7x2ma)", "`4be1c9d`", "`7f3cq2xz`", "event 42", "neither approves nor blocks"} {
		if !strings.Contains(public, want) {
			t.Errorf("the public body lacks %q:\n%s", want, public)
		}
	}
	if strings.Count(public, "@Smana") != 1 || strings.Count(public, "```") != 2 {
		t.Error("a summary never pings anyone: its one mention is inside the code block")
	}
	approve := v
	approve.Verdict = "approve"
	if !strings.Contains(Body(ev, approve, "public", ""), "### Agent review: approved") {
		t.Error("approve title")
	}
	spoof := v
	spoof.Text = "done\n```\n@Smana see Smana/other#3 ![x](https://evil.example/p.png) \u202eevil"
	b := Body(ev, spoof, "public", "https://rooms.priv.aws.ogenki.io")
	if strings.Contains(b, "\u202e") || !strings.Contains(b, "````\ndone\n```\n@Smana see Smana/other#3 ![x](https://evil.example/p.png) evil\n````\n") {
		t.Errorf("a summary is quoted in a block it cannot close (reviews M5, 3.5 m3):\n%s", b)
	}
	internal := Body(ev, v, "internal", "https://rooms.priv.aws.ogenki.io")
	if strings.Contains(internal, "Missing a test") || !strings.Contains(internal, "/r/3kq7x2ma") {
		t.Errorf("an internal room's summary stays in the room:\n%s", internal)
	}
	if unknown := Body(ev, v, "", ""); strings.Contains(unknown, "Missing a test") {
		t.Error("a room whose data class is unknown is never treated as public")
	}
}

func TestMarker(t *testing.T) {
	if got := Marker("3kq7x2ma", 42); got != "<!-- agent-room:3kq7x2ma:42 -->" {
		t.Fatal(got)
	}
}

// Ruling TC: a verdict that keeps failing waits 15 s, then twice as long each
// time, up to 15 min; a success forgets it.
func TestATransientFailureBacksOff(t *testing.T) {
	log, results := &fakeLog{verdicts: []envelope.Event{verdict(42, pr)}}, []string{}
	gh := &fakeGitHub{enabled: true, err: &github.APIError{Status: 502}}
	p, c := poster(log, gh, &results)
	want := []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute,
		8 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	_ = p.Once(t.Context())
	for i, d := range want {
		calls := len(gh.calls)
		c.t = c.t.Add(d - time.Second)
		_ = p.Once(t.Context())
		if len(gh.calls) != calls {
			t.Fatalf("failure %d: retried %s early", i+1, time.Second)
		}
		c.t = c.t.Add(time.Second)
		_ = p.Once(t.Context())
		if len(gh.calls) != calls+1 {
			t.Fatalf("failure %d: not retried after %s", i+1, d)
		}
	}
	gh.err = nil
	c.t = c.t.Add(15 * time.Minute)
	_ = p.Once(t.Context())
	if len(log.records) != 1 || len(p.held) != 0 {
		t.Fatalf("%d records, %d held after the success", len(log.records), len(p.held))
	}
}

// Held verdicts are excluded from the batch, so twenty stuck ones never
// starve a new one (review 3.5 m2).
func TestStuckVerdictsDoNotStarveNewOnes(t *testing.T) {
	log, results := &fakeLog{}, []string{}
	for seq := range int64(batch) {
		log.verdicts = append(log.verdicts, verdict(seq+1, pr))
	}
	gh := &fakeGitHub{enabled: true, err: &github.APIError{Status: 502}}
	p, c := poster(log, gh, &results)
	// Hold all twenty: two failing calls a tick, the tick stopping early.
	for len(p.held) < batch {
		_ = p.Once(t.Context())
	}
	log.verdicts = append(log.verdicts, verdict(99, pr))
	gh.err = nil
	calls := len(gh.calls)
	c.t = c.t.Add(time.Second)
	_ = p.Once(t.Context())
	if len(gh.calls) != calls+1 || !strings.Contains(gh.calls[len(gh.calls)-1], ":99 -->") {
		t.Fatalf("the new verdict was not posted: %d calls", len(gh.calls)-calls)
	}
	if len(log.excluded) != batch {
		t.Fatalf("%d excluded, want the %d held", len(log.excluded), batch)
	}
}

// Two failures in a row mean GitHub itself is failing: the tick stops rather
// than burn the batch on one outage.
func TestAnOutageStopsTheTick(t *testing.T) {
	log, results := &fakeLog{}, []string{}
	for seq := range int64(5) {
		log.verdicts = append(log.verdicts, verdict(seq+1, pr))
	}
	gh := &fakeGitHub{enabled: true, err: errors.New("dial tcp: connection refused")}
	p, _ := poster(log, gh, &results)
	_ = p.Once(t.Context())
	if len(gh.calls) != 2 {
		t.Fatalf("%d calls in one tick of an outage", len(gh.calls))
	}
	// A success between two failures resets the count.
	log2, gh2 := &fakeLog{}, &fakeGitHub{enabled: true}
	for seq := range int64(5) {
		log2.verdicts = append(log2.verdicts, verdict(seq+1, pr))
	}
	n := 0
	gh2.onCall = func() {
		n++
		gh2.err = nil
		if n%2 == 1 {
			gh2.err = &github.APIError{Status: 502}
		}
	}
	p2, _ := poster(log2, gh2, &results)
	_ = p2.Once(t.Context())
	if len(gh2.calls) != 5 {
		t.Fatalf("%d calls: alternating failures never stop the tick", len(gh2.calls))
	}
}

// The held map is bounded: past maxHeld, a failing verdict is not tracked,
// and the entries of verdicts out of the window are dropped.
func TestTheHeldMapIsBounded(t *testing.T) {
	log, results := &fakeLog{}, []string{}
	p, c := poster(log, &fakeGitHub{enabled: true}, &results)
	for i := range maxHeld + 10 {
		p.hold(envelope.Event{ID: fmt.Sprintf("e%d", i), TS: c.t}, c.t)
	}
	if len(p.held) != maxHeld {
		t.Fatalf("%d held, cap %d", len(p.held), maxHeld)
	}
	c.t = c.t.Add(Window + time.Second)
	_ = p.Once(t.Context())
	if len(p.held) != 0 {
		t.Fatalf("%d held for verdicts out of the window", len(p.held))
	}
}

// Review 3.5 m2b: a verdict that ages out of the window is recorded once as
// expired, even before the App key lands, so the room shows why.
func TestAnExpiredVerdictIsRecordedOnce(t *testing.T) {
	log, results := &fakeLog{expired: []envelope.Event{verdict(7, pr)}}, []string{}
	gh := &fakeGitHub{}
	p, c := poster(log, gh, &results)
	_ = p.Once(t.Context())
	_ = p.Once(t.Context())
	if len(log.records) != 1 || len(gh.calls) != 0 || !log.before.Equal(c.t.Add(-Window)) {
		t.Fatalf("%d records, %d calls, before %v", len(log.records), len(gh.calls), log.before)
	}
	if rec := record(t, log.records[0]); rec["kind"] != "verdict_not_posted" || rec["reason"] != "expired" || rec["detail"] != "window_24h" {
		t.Fatalf("record = %v", rec)
	}
	p.Leading = func() bool { return false }
	log.expired = append(log.expired, verdict(8, pr))
	_ = p.Once(t.Context())
	if len(log.records) != 1 {
		t.Fatal("recorded without the lease")
	}
}

// The mutant no-post-timeout: a post GitHub never answers ends at PostTimeout.
func TestAHungPostEndsAtItsTimeout(t *testing.T) {
	log, results := &fakeLog{verdicts: []envelope.Event{verdict(42, pr)}}, []string{}
	gh := &fakeGitHub{enabled: true, hang: true}
	p, _ := poster(log, gh, &results)
	p.PostTimeout = 50 * time.Millisecond
	done := make(chan struct{})
	go func() { _ = p.Once(t.Context()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a hung post held the tick")
	}
	if strings.Join(results, ",") != "error" || len(log.records) != 0 {
		t.Fatalf("results %v, %d records", results, len(log.records))
	}
	if (&Poster{}).postTimeout() != time.Minute {
		t.Fatal("the default post timeout is a minute")
	}
}
