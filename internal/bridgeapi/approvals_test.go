// SPDX-License-Identifier: Apache-2.0

package bridgeapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/store"
	"github.com/Smana/agent-platform/internal/wire"
)

// fakeApprovals records requests; err fails them, and prompters' err fails the read.
type fakeApprovals struct {
	mu          sync.Mutex
	got         []store.Approval
	drafts      []envelope.Draft
	err         error
	prompters   []string
	promptErr   error
	prompterFor []string // room/run of each Prompters call
}

func (f *fakeApprovals) RequestApproval(_ context.Context, a store.Approval, d envelope.Draft) (store.Approval, envelope.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return store.Approval{}, envelope.Event{}, f.err
	}
	f.got, f.drafts = append(f.got, a), append(f.drafts, d)
	a.State = store.ApprovalPending
	return a, envelope.Event{Seq: 7}, nil
}

func (f *fakeApprovals) Prompters(_ context.Context, roomID, runID string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompterFor = append(f.prompterFor, roomID+"/"+runID)
	return slices.Clone(f.prompters), f.promptErr
}

func request(eventID, callID, class string, action any) map[string]any {
	return map[string]any{"eventId": eventID, "callId": callID, "class": class, "action": action}
}

// POST /v1/bridge/approvals (§6, 5.2 contracts 1 and 2): validated, redacted,
// expiring by the room's profile, naming the run's prompters.
func TestApprovalRequests(t *testing.T) {
	s, _, w := newServer(t)
	run := agentRun(runA, room, "Running")
	run.Object["spec"].(map[string]any)["principal"] = "human:own"
	w.Upsert(t.Context(), run)
	f := &fakeApprovals{prompters: []string{"human:steerer"}}
	s.Approvals = f
	s.Limits = Limits{Rate: 1000, Burst: 1000, InFlight: 10}
	h := s.Routes()
	post := func(token string, body any) *httptest.ResponseRecorder {
		return call(t, h, http.MethodPost, "/v1/bridge/approvals", token, body)
	}

	start := time.Now()
	rec := post("run:"+runA, request("e42", "call_97", "forge.pr", map[string]any{"command": "gh pr create", "token": "ghp_" + "Zq3vR8kT1mW5xY9bN2cL7dF4gH6jK0pS8uE3"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("request: %d %s", rec.Code, rec.Body)
	}
	var ack wire.ApprovalAck
	if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil || ack.ApprovalID == "" || ack.ExpiresAt.IsZero() {
		t.Fatalf("ack %s (%v)", rec.Body, err)
	}
	a, d := f.got[0], f.drafts[0]
	if ack.ApprovalID != a.ID || a.RoomID != room || a.RunID != runA || a.EventID != "e42" || a.CallID != "call_97" || a.Class != "forge.pr" {
		t.Fatalf("approval %+v", a)
	}
	if wait := a.ExpiresAt.Sub(start); wait < 30*time.Minute || wait > 31*time.Minute {
		t.Fatalf("an attended approval waits %v, want 30 min", wait)
	}
	if strings.Contains(string(a.Action), "ghp_") || !strings.Contains(string(a.Action), "[REDACTED:github-pat]") ||
		!slices.Equal(d.Redactions, []string{"github-pat"}) {
		t.Fatalf("the card's action is redacted (T3): %s %v", a.Action, d.Redactions)
	}
	if !slices.Equal(a.Prompters, []string{"human:steerer", "human:own"}) || f.prompterFor[0] != room+"/"+runA {
		t.Fatalf("prompters %v (%v)", a.Prompters, f.prompterFor)
	}
	if d.Actor != (envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:" + runA, Role: "implementer"}) ||
		d.Origin != envelope.OriginHarness || d.OriginClient != "agent:"+runA+":approvals" || d.OriginSeq <= 0 {
		t.Fatalf("draft %+v", d)
	}

	t.Run("the principal is named once", func(t *testing.T) {
		f.prompters = []string{"human:own"}
		if rec := post("run:"+runA, request("e43", "c", "shell.high", map[string]any{})); rec.Code != http.StatusOK {
			t.Fatal(rec.Code)
		}
		if got := f.got[len(f.got)-1].Prompters; !slices.Equal(got, []string{"human:own"}) {
			t.Fatalf("prompters %v", got)
		}
	})

	// 5.2 contract 6: an action with no readable tool call id is named event:<id>.
	t.Run("an event: call id, and one of 256 bytes", func(t *testing.T) {
		for _, id := range []string{"event:e9", strings.Repeat("c", 256)} {
			if rec := post("run:"+runA, request("e44", id, "forge.other", map[string]any{})); rec.Code != http.StatusOK {
				t.Fatalf("%.20s: %d %s", id, rec.Code, rec.Body)
			}
			if got := f.got[len(f.got)-1]; got.CallID != id || got.EventID != "e44" {
				t.Fatalf("approval %+v", got)
			}
		}
	})

	t.Run("the room's profile sets the deadline", func(t *testing.T) {
		for _, c := range []struct {
			policy *wire.ApprovalPolicy
			want   time.Duration
		}{
			{nil, 30 * time.Minute},
			{&wire.ApprovalPolicy{Profile: "attended", TTL: "90m"}, 30 * time.Minute},
			{&wire.ApprovalPolicy{Profile: ""}, 30 * time.Minute},
			{&wire.ApprovalPolicy{Profile: "unattended", TTL: "90m"}, 90 * time.Minute},
			{&wire.ApprovalPolicy{Profile: "unattended"}, 4 * time.Hour},
			{&wire.ApprovalPolicy{Profile: "unattended", TTL: "soon"}, 4 * time.Hour},
			{&wire.ApprovalPolicy{Profile: "unattended", TTL: "-5m"}, 4 * time.Hour},
		} {
			s.RoomPolicy = nil
			if c.policy != nil {
				p := *c.policy
				s.RoomPolicy = func(id string) wire.ApprovalPolicy {
					if id != room {
						t.Fatalf("policy of %s", id)
					}
					return p
				}
			}
			start := time.Now()
			if rec := post("run:"+runA, request("e50", "c", "forge.push", map[string]any{})); rec.Code != http.StatusOK {
				t.Fatal(rec.Code)
			}
			if wait := f.got[len(f.got)-1].ExpiresAt.Sub(start); wait < c.want || wait > c.want+time.Minute {
				t.Errorf("%+v waits %v, want %v", c.policy, wait, c.want)
			}
		}
		s.RoomPolicy = nil
	})

	t.Run("refusals", func(t *testing.T) {
		n := len(f.got)
		for _, c := range []struct {
			name, token string
			body        any
			code        int
			reason      string
		}{
			{"no token", "", request("e", "c", "forge.pr", map[string]any{}), 401, wire.ReasonUnauthenticated},
			{"a run that is not live", "run:" + runB, request("e", "c", "forge.pr", map[string]any{}), 403, wire.ReasonRunNotLive},
			{"no event id", "run:" + runA, request("", "c", "forge.pr", map[string]any{}), 400, wire.ReasonBadApproval},
			{"no call id", "run:" + runA, request("e", "", "forge.pr", map[string]any{}), 400, wire.ReasonBadApproval},
			{"a long event id", "run:" + runA, request(strings.Repeat("e", 257), "c", "forge.pr", map[string]any{}), 400, wire.ReasonBadApproval},
			{"a long call id", "run:" + runA, request("e", strings.Repeat("c", 257), "forge.pr", map[string]any{}), 400, wire.ReasonBadApproval},
			{"egress is never approvable", "run:" + runA, request("e", "c", "egress.new", map[string]any{}), 400, wire.ReasonBadApproval},
			{"plain needs nobody", "run:" + runA, request("e", "c", "plain", map[string]any{}), 400, wire.ReasonBadApproval},
			{"an unknown field", "run:" + runA, map[string]any{"eventId": "e", "callId": "c", "class": "forge.pr", "action": map[string]any{}, "allow": true}, 400, wire.ReasonBadApproval},
			{"not JSON", "run:" + runA, []byte("{"), 400, wire.ReasonBadApproval},
			{"no action", "run:" + runA, map[string]any{"eventId": "e", "callId": "c", "class": "forge.pr"}, 400, wire.ReasonBadAction},
			{"an action that is no object", "run:" + runA, request("e", "c", "forge.pr", "gh pr merge 1"), 400, wire.ReasonBadAction},
			{"a null action", "run:" + runA, request("e", "c", "forge.pr", nil), 400, wire.ReasonBadAction},
			{"keys that collide once redacted", "run:" + runA, []byte(`{"eventId":"e","callId":"c","class":"forge.pr","action":{"ghp_Zq3vR8kT1mW5xY9bN2cL7dF4gH6jK0pS8uE3":1,"ghp_Pw7nB4xQ2sV9tC6yM1hJ8kD3fG5rL0aZ4eU7":2}}`), 400, wire.ReasonBadAction},
			{"an action over the cap", "run:" + runA, request("e", "c", "forge.pr", map[string]string{"x": strings.Repeat("y", maxApprovalAction)}), 400, wire.ReasonBadAction},
			{"a body over 64 KiB", "run:" + runA, request("e", "c", "forge.pr", map[string]string{"x": strings.Repeat("y", maxApprovalBytes)}), 400, wire.ReasonBadApproval},
		} {
			t.Run(c.name, func(t *testing.T) {
				if rec := post(c.token, c.body); rec.Code != c.code || reason(t, rec) != c.reason {
					t.Fatalf("%d %s, want %d %s", rec.Code, rec.Body, c.code, c.reason)
				}
			})
		}
		if len(f.got) != n {
			t.Fatalf("a refused request was recorded: %d", len(f.got)-n)
		}
	})

	t.Run("failures", func(t *testing.T) {
		f.promptErr = errors.New("down")
		n := len(f.got)
		if rec := post("run:"+runA, request("e60", "c", "forge.pr", map[string]any{})); rec.Code != http.StatusServiceUnavailable ||
			reason(t, rec) != wire.ReasonLogUnavailable || len(f.got) != n {
			t.Fatalf("unreadable prompters never weaken four-eyes: %d %s", rec.Code, rec.Body)
		}
		f.promptErr = nil
		for _, c := range []struct {
			err    error
			code   int
			reason string
		}{
			{store.ErrLeaseLost, http.StatusConflict, wire.ReasonLeaseLost},
			{store.ErrSealed, http.StatusGone, wire.ReasonSealed},
			{errors.New("down"), http.StatusServiceUnavailable, wire.ReasonLogUnavailable},
		} {
			f.err = c.err
			if rec := post("run:"+runA, request("e61", "c", "forge.pr", map[string]any{})); rec.Code != c.code || reason(t, rec) != c.reason {
				t.Errorf("%v: %d %s", c.err, rec.Code, rec.Body)
			}
		}
		f.err = nil
		s.Approvals = nil
		if rec := post("run:"+runA, request("e62", "c", "forge.pr", map[string]any{})); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("no approvals store: %d", rec.Code)
		}
		s.Approvals = f
	})

	t.Run("the run's limits apply", func(t *testing.T) {
		limited := &Server{Log: s.Log, Redactor: s.Redactor, Runs: s.Runs, Systems: s.Systems, Watch: s.Watch, Approvals: f,
			Limits: Limits{Rate: 0.001, Burst: 1, InFlight: 1}, Logger: s.Logger}
		lh := limited.Routes()
		if rec := call(t, lh, http.MethodPost, "/v1/bridge/approvals", "run:"+runA, request("e70", "c", "forge.pr", map[string]any{})); rec.Code != http.StatusOK {
			t.Fatal(rec.Code)
		}
		if rec := call(t, lh, http.MethodPost, "/v1/bridge/approvals", "run:"+runA, request("e71", "c", "forge.pr", map[string]any{})); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("over the run's rate: %d", rec.Code)
		}
	})
}

func decided(seq int64, run, decision, why string) envelope.Event {
	return envelope.Event{Seq: seq, RunID: run, Type: envelope.ApprovalDecided,
		Payload: envelope.Must(envelope.ApprovalDecidedPayload{ApprovalID: "ap1", Decision: decision, Reason: why})}
}

// 5.2 contracts 3 and 4: a decision of the run's approval is a decision frame
// carrying its seq as ref; an expiry is allow false, reason expired; a superseded
// approval's call already has its result, so nothing is sent.
func TestDecisionsAreDeliverable(t *testing.T) {
	for _, c := range []struct {
		name string
		ev   envelope.Event
		want string
	}{
		{"approved", decided(51, runA, "approved", ""), `{"approvalId":"ap1","allow":true,"ref":51}`},
		{"denied, with the approver's reason", decided(52, runA, "denied", "not this branch"), `{"approvalId":"ap1","allow":false,"reason":"not this branch","ref":52}`},
		{"expired", decided(53, runA, "expired", "no decision before the deadline"), `{"approvalId":"ap1","allow":false,"reason":"expired","ref":53}`},
		{"superseded", decided(54, runA, "superseded", "the call already has a result"), ""},
		{"another run's", decided(55, runB, "approved", ""), ""},
		{"an unknown outcome", decided(56, runA, "maybe", ""), ""},
		{"no approval id", envelope.Event{Seq: 57, RunID: runA, Type: envelope.ApprovalDecided, Payload: []byte(`{"decision":"approved"}`)}, ""},
		{"an unreadable payload", envelope.Event{Seq: 58, RunID: runA, Type: envelope.ApprovalDecided, Payload: []byte(`[]`)}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			name, data, ok := Deliverable(c.ev, runA)
			if c.want == "" {
				if ok {
					t.Fatalf("sent %s %s", name, data)
				}
				return
			}
			if !ok || name != wire.EventDecision || string(data) != c.want {
				t.Fatalf("%s %s %v, want %s", name, data, ok, c.want)
			}
		})
	}
}

// A decision reaches the run's stream from the log, and its decision_applied
// moves the stream's mark: a reconnect does not send it again.
func TestStreamDeliversDecisions(t *testing.T) {
	s, log, w := newServer(t)
	s.Ticker = make(manualTicker).new
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	appendDecision := func(n int64, decision string) {
		t.Helper()
		if _, _, err := log.Append(t.Context(), envelope.Draft{RoomID: room, RunID: runA,
			Actor: envelope.Actor{Kind: envelope.ActorHuman, ID: "human:own"}, Type: envelope.ApprovalDecided,
			Origin: envelope.OriginClient, OriginClient: "human:own:s1", OriginSeq: n,
			Payload: envelope.Must(envelope.ApprovalDecidedPayload{ApprovalID: "ap" + decision, Decision: decision})}); err != nil {
			t.Fatal(err)
		}
	}
	appendDecision(1, "approved")
	appendDecision(2, "superseded")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	sc, stop := openStream(t, srv, runA)
	defer stop()
	defer time.AfterFunc(5*time.Second, stop).Stop()
	expectFrame(t, sc, wire.EventDecision, `{"approvalId":"apapproved","allow":true,"ref":1}`)
	expectPing(t, sc)
	appendDecision(3, "denied")
	expectFrame(t, sc, wire.EventDecision, `{"approvalId":"apdenied","allow":false,"ref":3}`)
	stop()
	if _, _, err := log.Append(t.Context(), envelope.Draft{RoomID: room, RunID: runA,
		Actor: envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:" + runA}, Type: envelope.StateChanged,
		Origin: envelope.OriginHarness, OriginClient: "agent:" + runA + ":status", OriginSeq: 1,
		Payload: envelope.StatePayload("decision_applied", map[string]any{"ref": 3, "runId": runA})}); err != nil {
		t.Fatal(err)
	}
	sc2, stop2 := openStream(t, srv, runA)
	defer stop2()
	defer time.AfterFunc(5*time.Second, stop2).Stop()
	expectPing(t, sc2) // both decisions acknowledged up to ref 3: nothing replays
}
