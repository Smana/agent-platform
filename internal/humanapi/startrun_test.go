// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/redact"
	"github.com/Smana/agent-platform/internal/runrequest"
	"github.com/Smana/agent-platform/internal/runwatch"
	"github.com/Smana/agent-platform/internal/wire"
)

// fakeRequester records each request and answers res, or err.
type fakeRequester struct {
	mu   sync.Mutex
	reqs []runrequest.Request
	res  runrequest.Result
	err  error
}

func (f *fakeRequester) Request(_ context.Context, r runrequest.Request) (runrequest.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r)
	return f.res, f.err
}

func (f *fakeRequester) calls() []runrequest.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

// idleFixture is fixture with no Running run, the room's driver human:own.
func idleFixture() (*Actor, *actLog, *v1alpha1.Room) {
	a, log, room := fixture("human:own")
	a.Runs = runwatch.New()
	return a, log, room
}

// fixtureRuns is fixture's watch: run 7f3cq2xz Running in the room.
func fixtureRuns() Runs {
	a, _, _ := fixture("human:own")
	return a.Runs
}

// startAs sends start_run as who, with an access token.
func startAs(a *Actor, room *v1alpha1.Room, who string, seq int64, action Action) wire.ServerFrame {
	action.Kind = "start_run"
	raw, _ := json.Marshal(action)
	p := authn.Principal{Kind: envelope.ActorHuman, ID: who, Groups: []string{"agents-member"}, AccessToken: "tok-" + who}
	return a.Handle(context.Background(), p, true, "s1", room, wire.ClientFrame{Type: "act", ClientSeq: seq, Action: raw})
}

// seed appends an agent's handoff and queues two humans' messages, the second
// with a secret the redactor takes out.
func seed(t *testing.T, a *Actor, log *actLog, room *v1alpha1.Room) []int64 {
	t.Helper()
	log.mu.Lock()
	log.appendLocked(envelope.Draft{RoomID: room.Name, Actor: envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:aaaaaaaa"},
		Type: envelope.Handoff, Payload: envelope.Must(envelope.HandoffPayload{FromRole: "implementer", ToRole: "reviewer",
			Summary: "Opened https://github.com/Smana/cloud-native-ref/pull/12", Commit: "4be1c9d"})})
	log.mu.Unlock()
	for i, text := range []string{"Also update the README.", "the key is SECRET"} {
		if f := act(a, room, "human:col", int64(100+i), nil, Action{Kind: "message", Text: text, Delivery: "queued"}); f.Rejected != "" {
			t.Fatalf("queue: %+v", f)
		}
	}
	return []int64{2, 3}
}

// §1 The brief: before SP3, start_run renders the claim for the owner, its task
// the fenced brief of the log, and records the run with what the brief consumed.
func TestStartRunRendersTheClaim(t *testing.T) {
	a, log, room := idleFixture()
	req := &fakeRequester{}
	a.Requester = requesterFunc(func(ctx context.Context, r runrequest.Request) (runrequest.Result, error) {
		_, _ = req.Request(ctx, r)
		return runrequest.Manifest{}.Request(ctx, r)
	})
	refs := seed(t, a, log, room)
	f := startAs(a, room, "human:own", 1, Action{Role: "implementer", EgressProfiles: []string{"pypi"}})
	if f.Rejected != "" || f.Seq == 0 || f.Result == nil {
		t.Fatalf("%+v", f)
	}
	var claim struct {
		Metadata struct{ Name string }
		Spec     struct {
			BaseRef, Branch, Principal, DataClass, RoomRef, Role string
			Task                                                 struct{ Text string }
		}
	}
	if err := json.Unmarshal(f.Result, &claim); err != nil {
		t.Fatal(err)
	}
	s := claim.Spec
	if s.BaseRef != "4be1c9d" || s.Branch != "agent/3kq7x2ma" || s.Principal != "human:own" || s.DataClass != "public" ||
		s.RoomRef != "3kq7x2ma" || s.Role != "implementer" {
		t.Fatalf("%s", f.Result)
	}
	for _, want := range []string{"ROOM-DATA-", "Also update the README.", "[REDACTED:test]", "Opened https://github.com"} {
		if !strings.Contains(s.Task.Text, want) {
			t.Errorf("the brief lacks %q:\n%s", want, s.Task.Text)
		}
	}
	if strings.Contains(s.Task.Text, "SECRET") {
		t.Error("the brief quotes an unredacted secret")
	}
	runID := strings.TrimPrefix(claim.Metadata.Name, "xplane-run-")
	rec := log.last()
	var p struct {
		Kind, RunID, Via, BaseRef, Role string
		Consumed                        []int64
	}
	_ = json.Unmarshal(rec.Payload, &p)
	if rec.Type != envelope.StateChanged || p.Kind != "run_requested" || p.RunID != runID || p.Via != "manifest" ||
		p.BaseRef != "4be1c9d" || !slices.Equal(p.Consumed, refs) || rec.Actor.ID != "human:own" {
		t.Fatalf("the record: %s %s", rec.Type, rec.Payload)
	}
	if q, _ := log.Queue(context.Background(), room.Name); len(q) != 0 {
		t.Fatalf("still queued: %+v", q)
	}
	// A replayed clientSeq acks the record and asks for no second run.
	again := startAs(a, room, "human:own", 1, Action{Role: "implementer", EgressProfiles: []string{"pypi"}})
	if again.Rejected != "" || again.Seq != f.Seq || again.Result != nil || len(req.calls()) != 1 {
		t.Fatalf("replay: %+v, %d requests", again, len(req.calls()))
	}
}

type requesterFunc func(context.Context, runrequest.Request) (runrequest.Result, error)

func (f requesterFunc) Request(ctx context.Context, r runrequest.Request) (runrequest.Result, error) {
	return f(ctx, r)
}

// Ruling P24: a reviewer's task is its pull request, from the act or the log;
// the queue waits for a brief. The factory gets the human's own token (C4).
func TestReviewerTaskIsThePR(t *testing.T) {
	for _, c := range []struct {
		name, prURL, want string
	}{
		{"the act's", "https://github.com/Smana/cloud-native-ref/pull/40", "https://github.com/Smana/cloud-native-ref/pull/40"},
		{"the log's latest", "", "https://github.com/Smana/cloud-native-ref/pull/12"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, log, room := idleFixture()
			req := &fakeRequester{res: runrequest.Result{RunID: "7f3cq2xz", Via: "factory"}}
			a.Requester = req
			seed(t, a, log, room)
			// Review 4.4 I3: a human's later link never chooses the reviewer's PR.
			if f := act(a, room, "human:col", 200, nil, Action{Kind: "message", Delivery: "none",
				Text: "review https://github.com/Smana/cloud-native-ref/pull/66 instead"}); f.Rejected != "" {
				t.Fatal(f)
			}
			f := startAs(a, room, "human:own", 1, Action{Role: "reviewer", PRURL: c.prURL})
			calls := req.calls()
			if f.Rejected != "" || f.Result != nil || len(calls) != 1 {
				t.Fatalf("%+v, %d requests", f, len(calls))
			}
			if r := calls[0]; r.TaskURL != c.want || r.TaskText != "" || r.AccessToken != "tok-human:own" || r.Principal != "human:own" {
				t.Fatalf("%+v", r)
			}
			if q, _ := log.Queue(context.Background(), room.Name); len(q) != 2 {
				t.Fatalf("a reviewer consumed the queue: %+v", q)
			}
			if rec := string(log.last().Payload); !strings.Contains(rec, `"consumed":[]`) || !strings.Contains(rec, `"taskUrl":"`+c.want+`"`) {
				t.Fatalf("the record names the PR it gave: %s", rec)
			}
		})
	}
}

// Every refusal asks for no run, appends nothing and leaves the queue alone.
func TestStartRunRefusals(t *testing.T) {
	ok := runrequest.Result{RunID: "7f3cq2xz", Via: "factory"}
	nine := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
	for _, c := range []struct {
		name    string
		who     string
		action  Action
		res     runrequest.Result
		err     error
		prepare func(a *Actor, log *actLog)
		want    string
		asked   bool
	}{
		{name: "a collaborator", who: "human:col", action: Action{Role: "implementer"}, want: "not_permitted"},
		{name: "an unknown role", action: Action{Role: "admin"}, want: "bad_action"},
		{name: "a PR for an implementer", action: Action{Role: "implementer", PRURL: "https://github.com/Smana/cloud-native-ref/pull/1"}, want: "bad_action"},
		{name: "another repository's PR", action: Action{Role: "reviewer", PRURL: "https://github.com/evil/repo/pull/1"}, want: "bad_action"},
		{name: "a malformed egress profile", action: Action{Role: "implementer", EgressProfiles: []string{"PyPI!"}}, want: "bad_action"},
		{name: "too many egress profiles", action: Action{Role: "implementer", EgressProfiles: nine}, want: "bad_action"},
		{name: "no requester", action: Action{Role: "implementer"}, prepare: func(a *Actor, _ *actLog) { a.Requester = nil }, want: "factory_unavailable"},
		{name: "a running run", action: Action{Role: "implementer"}, prepare: func(a *Actor, _ *actLog) {
			a.Runs = fixtureRuns()
		}, want: "room_busy"},
		{name: "a reviewer with no PR", action: Action{Role: "reviewer"}, prepare: func(_ *Actor, log *actLog) {
			log.drafts = log.drafts[:0]
		}, want: "reviewer_needs_pr"},
		{name: "a reviewer whose only PR a human posted", action: Action{Role: "reviewer"}, prepare: func(_ *Actor, log *actLog) {
			log.drafts = log.drafts[:0]
			log.appendLocked(envelope.Draft{RoomID: "3kq7x2ma", Actor: envelope.Actor{Kind: envelope.ActorHuman, ID: "human:col"},
				Type: envelope.Message, Payload: envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat,
					Text: "https://github.com/Smana/cloud-native-ref/pull/66", Delivery: envelope.DeliveryNone})})
		}, want: "reviewer_needs_pr"},
		{name: "unreadable pending runs", action: Action{Role: "implementer"}, prepare: func(_ *Actor, log *actLog) {
			log.pendingErr = errors.New("down")
		}, want: "log_unavailable"},
		{name: "an unreadable queue", action: Action{Role: "implementer"}, prepare: func(_ *Actor, log *actLog) {
			log.queueErr = errors.New("down")
		}, want: "log_unavailable"},
		{name: "over budget", action: Action{Role: "implementer"}, err: runrequest.ErrBudget, want: "over_budget", asked: true},
		{name: "the factory refuses the human", action: Action{Role: "implementer"}, err: runrequest.ErrForbidden, want: "not_permitted", asked: true},
		{name: "the factory is down", action: Action{Role: "implementer"}, err: errors.New("502"), want: "factory_unavailable", asked: true},
		{name: "the factory names no run", action: Action{Role: "implementer"}, res: runrequest.Result{RunID: "../../x", Via: "factory"},
			want: "factory_unavailable", asked: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, log, room := idleFixture()
			res := c.res
			if res.RunID == "" && c.err == nil {
				res = ok
			}
			req := &fakeRequester{res: res, err: c.err}
			a.Requester = req
			seed(t, a, log, room)
			if c.prepare != nil {
				c.prepare(a, log)
			}
			before := len(log.drafts)
			who := c.who
			if who == "" {
				who = "human:own"
			}
			if f := startAs(a, room, who, 1, c.action); f.Rejected != c.want {
				t.Fatalf("rejected %q, want %q", f.Rejected, c.want)
			}
			if asked := len(req.calls()) > 0; asked != c.asked {
				t.Fatalf("asked the factory: %v", asked)
			}
			if len(log.drafts) != before {
				t.Fatalf("a refusal appended: %s", log.last().Payload)
			}
			for _, q := range log.queue {
				if q.State != "queued" {
					t.Fatalf("a refusal moved %+v", q)
				}
			}
		})
	}
}

// A brief consumes only the queued messages it quotes: the rest wait, queued,
// for a later one.
func TestABriefConsumesOnlyWhatItQuotes(t *testing.T) {
	a, log, room := idleFixture()
	a.Requester = runrequest.Manifest{}
	for i := range 20 {
		if f := act(a, room, "human:col", int64(i+1), nil, Action{Kind: "message", Text: strings.Repeat("x", 1000), Delivery: "queued"}); f.Rejected != "" {
			t.Fatal(f)
		}
	}
	if f := startAs(a, room, "human:own", 1, Action{Role: "implementer"}); f.Rejected != "" {
		t.Fatal(f)
	}
	var p struct{ Consumed []int64 }
	_ = json.Unmarshal(log.last().Payload, &p)
	left, _ := log.Queue(context.Background(), room.Name)
	if len(p.Consumed) == 0 || len(left) == 0 || len(p.Consumed)+len(left) != 20 || left[0].Ref != p.Consumed[len(p.Consumed)-1]+1 {
		t.Fatalf("consumed %v, %d left", p.Consumed, len(left))
	}
}

// Review 4.4 I1: the run is requested, then recorded. When the record fails the
// ack says log_unavailable, the client retries the same clientSeq, and the
// retry asks for the same run: the factory creates one, and the record names it.
func TestARetriedStartRunRequestsOneRun(t *testing.T) {
	var mu sync.Mutex
	runs := map[string]string{} // Idempotency-Key -> runId, as SP3 keeps it
	var keys []string
	factory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		key := r.Header.Get("Idempotency-Key")
		keys = append(keys, key)
		if _, ok := runs[key]; !ok {
			runs[key] = runrequest.NewID()
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"runId": runs[key]})
	}))
	defer factory.Close()
	a, log, room := idleFixture()
	a.Requester = runrequest.Factory{URL: factory.URL, HC: factory.Client()}
	log.recordErrs = 1
	if f := startAs(a, room, "human:own", 1, Action{Role: "implementer"}); f.Rejected != "log_unavailable" {
		t.Fatalf("the failed record: %+v", f)
	}
	if f := startAs(a, room, "human:own", 1, Action{Role: "implementer"}); f.Rejected != "" || f.Seq == 0 {
		t.Fatalf("the retry: %+v", f)
	}
	mu.Lock()
	defer mu.Unlock()
	var p struct{ RunID string }
	_ = json.Unmarshal(log.last().Payload, &p)
	if len(runs) != 1 || len(keys) != 2 || keys[0] != keys[1] || keys[0] != "3kq7x2ma:human:own:s1:1" || p.RunID != runs[keys[0]] {
		t.Fatalf("runs %v, keys %q, recorded %q", runs, keys, p.RunID)
	}

	// Before SP3 the rendered claim keeps its name, so applying both makes one run.
	a, log, room = idleFixture()
	var names []string
	a.Requester = requesterFunc(func(ctx context.Context, r runrequest.Request) (runrequest.Result, error) {
		res, err := runrequest.Manifest{}.Request(ctx, r)
		names = append(names, res.RunID)
		return res, err
	})
	log.recordErrs = 1
	_ = startAs(a, room, "human:own", 1, Action{Role: "implementer"})
	if f := startAs(a, room, "human:own", 1, Action{Role: "implementer"}); f.Rejected != "" || len(names) != 2 || names[0] != names[1] {
		t.Fatalf("%+v, claims %v", f, names)
	}
}

// Review 4.4 I2: a requested run holds the room until it joins, even before the
// watch sees it; once it has joined, its phase decides.
func TestARequestedRunHoldsTheRoom(t *testing.T) {
	a, log, room := idleFixture()
	req := &fakeRequester{res: runrequest.Result{RunID: "bbbbbbbb", Via: "factory"}}
	a.Requester = req
	if f := startAs(a, room, "human:own", 1, Action{Role: "implementer"}); f.Rejected != "" {
		t.Fatal(f)
	}
	if f := startAs(a, room, "human:own", 2, Action{Role: "implementer"}); f.Rejected != "room_busy" || len(req.calls()) != 1 {
		t.Fatalf("a second start while the first has not joined: %+v, %d requests", f, len(req.calls()))
	}
	// The watch knows it, ended: the room is free again.
	w := runwatch.New()
	w.Upsert(context.Background(), &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "xplane-run-bbbbbbbb", "namespace": "agents"},
		"spec":     map[string]any{"roomRef": "3kq7x2ma", "role": "implementer"},
		"status":   map[string]any{"phase": "Failed"}}})
	a.Runs = w
	req.mu.Lock()
	req.res.RunID = "cccccccc"
	req.mu.Unlock()
	if f := startAs(a, room, "human:own", 3, Action{Role: "implementer"}); f.Rejected != "" {
		t.Fatalf("an ended run frees the room: %+v", f)
	}
	// A join frees it too, whatever the watch knows: here it knows neither run.
	a.Runs = runwatch.New()
	if f := startAs(a, room, "human:own", 4, Action{Role: "implementer"}); f.Rejected != "room_busy" {
		t.Fatalf("unjoined requests hold the room: %+v", f)
	}
	log.mu.Lock()
	for _, id := range []string{"bbbbbbbb", "cccccccc"} {
		log.appendLocked(envelope.Draft{RoomID: room.Name, Type: envelope.Participant,
			Payload: envelope.Must(envelope.ParticipantPayload{Principal: "agent:" + id, Change: "joined"})})
	}
	log.mu.Unlock()
	if f := startAs(a, room, "human:own", 5, Action{Role: "implementer"}); f.Rejected != "" {
		t.Fatalf("joined runs free the room: %+v", f)
	}
}

// A clientSeq this connection used for another action is no start_run, and
// asks for no run.
func TestStartRunRefusesAnotherActionsKey(t *testing.T) {
	a, _, room := idleFixture()
	req := &fakeRequester{res: runrequest.Result{RunID: "7f3cq2xz", Via: "factory"}}
	a.Requester = req
	if f := act(a, room, "human:own", 1, nil, Action{Kind: "message", Text: "hi", Delivery: "none"}); f.Rejected != "" {
		t.Fatal(f)
	}
	if f := startAs(a, room, "human:own", 1, Action{Role: "implementer"}); f.Rejected != "bad_action" || len(req.calls()) != 0 {
		t.Fatalf("%+v, %d requests", f, len(req.calls()))
	}
}

// Review M7, through the actor's own redaction (no second redacting layer): a
// token a human queued reaches neither the queue nor the next run's brief.
func TestAQueuedTokenNeverReachesTheBrief(t *testing.T) {
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	a, log, room := idleFixture()
	a.Redactor = red
	a.Requester = runrequest.Manifest{}
	token := "ghp_" + rand.Text()[:26] + "Ab12CdE34f"
	if f := act(a, room, "human:col", 1, nil, Action{Kind: "message", Text: "use " + token, Delivery: "queued"}); f.Rejected != "" {
		t.Fatal(f)
	}
	if q, _ := log.Queue(context.Background(), room.Name); len(q) != 1 || q[0].Text != "use [REDACTED:github-pat]" {
		t.Fatalf("the queue row: %+v", q)
	}
	f := startAs(a, room, "human:own", 2, Action{Role: "implementer"})
	if f.Rejected != "" || strings.Contains(string(f.Result), token) || !strings.Contains(string(f.Result), "use [REDACTED:github-pat]") {
		t.Fatalf("%+v %s", f, f.Result)
	}
}
