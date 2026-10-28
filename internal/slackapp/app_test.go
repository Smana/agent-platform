// SPDX-License-Identifier: Apache-2.0

package slackapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/sponsored"
	"github.com/Smana/agent-platform/internal/webhook"
)

// F9, R22–R24, invariants 6–8, live proof 5 offline: a mention drafts; a ✅ from a linked
// maintainer files and starts the work once; from anyone else nothing is filed; a ✅ on a message
// that is not the App's draft does nothing; the thread receives the room's state as templates.
func TestReactionFlow(t *testing.T) {
	rig := newAppRig(t, Channel{Repositories: []string{"Smana/ai-native-ref"}, MaxCostUSD: 2})
	rig.mention("C03", "1729.0001", "<@U0BOT> Smana/ai-native-ref docs(agents): the link is dead\n## What is wrong\nx\n## What it should be\ny\n## Acceptance check\nz")
	draft := rig.lastPost()
	if draft.meta == nil || draft.meta.EventType != "factory_draft" || !strings.Contains(draft.text, "a maintainer replies :white_check_mark: or reacts :white_check_mark:") {
		t.Fatalf("%+v", draft)
	}
	rig.broker.refuse("slack:T01ABC/U09XYZ", "rejected: not_linked")
	rig.react("C03", draft.ts, "U09XYZ", "white_check_mark")
	if rig.broker.created() != 0 || !strings.Contains(rig.lastPost().text, "not_linked") {
		t.Fatal("an unlinked user's ✅ filed, or its refusal went unsaid")
	}
	if refused := rig.broker.refusedCalls(); len(refused) != 1 || refused[0].OnBehalfOf != "slack:T01ABC/U09XYZ" {
		t.Fatalf("the unlinked user's call: %+v", refused)
	}
	rig.react("C03", draft.ts, "U02DEF", "+1")
	rig.react("C03", "1729.0001", "U02DEF", "white_check_mark") // the human's own message, not a draft
	rig.react("C99", draft.ts, "U02DEF", "white_check_mark")    // a channel the App does not serve
	if rig.broker.created() != 0 {
		t.Fatal("only a ✅ on the App's draft in a served channel counts")
	}
	rig.react("C03", draft.ts, "U02DEF", "white_check_mark")
	rig.react("C03", draft.ts, "U07GHI", "white_check_mark") // a second maintainer: the same key, one task
	calls := rig.broker.runCreates()
	if len(calls) != 2 || calls[0].Key != calls[1].Key || calls[0].OnBehalfOf != "slack:T01ABC/U02DEF" || calls[0].MaxCostUSD != 2 ||
		calls[0].Title != "docs(agents): the link is dead" || calls[0].Ref != "slack/T01ABC/C03/1729.0001" {
		t.Fatalf("%+v", calls)
	}
	if !sponsored.ValidRef(calls[0].Ref) { // B1: the fake must not hide a ref the broker refuses (bad_ref)
		t.Fatalf("the broker refuses the ref %q", calls[0].Ref)
	}
	if th, ok := rig.threads.get("aaaaaaaa"); !ok || th.Channel != "C03" || th.TS != "1729.0001" {
		t.Fatalf("the thread map: %+v", th)
	}
	if !strings.Contains(rig.lastPost().text, "Already filed: https://rooms.example/r/aaaaaaaa") {
		t.Fatalf("%q", rig.lastPost().text)
	}
}

// A draft the broker refuses is said in the thread and carries no metadata: there is nothing to ✅.
func TestMentionRefusedStaysUnfiled(t *testing.T) {
	rig := newAppRig(t, Channel{Repositories: []string{"Smana/ai-native-ref"}, MaxCostUSD: 2})
	rig.broker.draftErr = Refused("rejected: missing_section")
	rig.mention("C03", "1729.0001", "<@U0BOT> Smana/ai-native-ref a title\nbody")
	p := rig.lastPost()
	if p.meta != nil || !strings.HasPrefix(p.text, "Draft refused: rejected: missing_section") {
		t.Fatalf("%+v", p)
	}
	// a repository the channel does not serve, a channel that is not served, and a bad first line
	rig.mention("C03", "1729.0003", "<@U0BOT> Smana/other-repo a title\nbody")
	rig.mention("C77", "1729.0004", "<@U0BOT> Smana/ai-native-ref a title\nbody")
	if n := len(rig.posts()); n != 2 {
		t.Fatalf("%d posts: an unserved repository or channel must not draft silently beyond one refusal", n)
	}
}

// A tampered draft (the hash does not match the metadata's own text) is never filed.
func TestReactionOnTamperedDraftIsIgnored(t *testing.T) {
	rig := newAppRig(t, Channel{Repositories: []string{"Smana/ai-native-ref"}, MaxCostUSD: 2})
	rig.slack.seed(Message{TS: "1729.0009", ThreadTS: "1729.0001", User: "U0BOT", Text: "draft",
		Metadata: &Metadata{EventType: "factory_draft", EventPayload: DraftPayload{Repo: "Smana/ai-native-ref", Title: "t", Body: "b", SHA256: "deadbeef"}}})
	rig.react("C03", "1729.0009", "U02DEF", "white_check_mark")
	if rig.broker.created() != 0 || len(rig.broker.all()) != 0 {
		t.Fatal("a draft whose hash does not match was filed")
	}
	// a message by someone else carrying draft-shaped metadata is not the App's draft
	rig.slack.seed(Message{TS: "1729.0010", ThreadTS: "1729.0001", User: "U02DEF", Text: "forged",
		Metadata: &Metadata{EventType: "factory_draft", EventPayload: draftPayload("Smana/ai-native-ref", "t", "b")}})
	rig.react("C03", "1729.0010", "U02DEF", "white_check_mark")
	if len(rig.broker.all()) != 0 {
		t.Fatal("a forged draft by another user was filed")
	}
}

// R26: Slack retries; an event_id seen is not handled twice. Unsigned requests are 401 and counted;
// url_verification echoes the challenge.
func TestEventsEnvelope(t *testing.T) {
	rig := newAppRig(t, Channel{Repositories: []string{"Smana/ai-native-ref"}, MaxCostUSD: 2})
	body := rig.eventBody("Ev-fixed", `{"type":"app_mention","channel":"C03","user":"U02DEF","ts":"1729.0001","text":"<@U0BOT> Smana/ai-native-ref t\nb"}`)
	for range 2 {
		if st := rig.postEvents(body, true).Code; st != 200 {
			t.Fatal(st)
		}
	}
	if len(rig.posts()) != 1 {
		t.Fatalf("a retried event_id drafted twice: %d posts", len(rig.posts()))
	}
	if st := rig.postEvents(body, false).Code; st != 401 {
		t.Fatalf("unsigned: %d", st)
	}
	if rig.metrics.count("events", "bad_signature") != 1 {
		t.Fatal("a bad signature went uncounted")
	}
	w := rig.postEvents(`{"type":"url_verification","challenge":"abc123"}`, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "abc123") {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
}

// R24, invariant 7: a thread line is a template of ids, states, codes and the room's URL.
func TestThreadMessagesCarryNoText(t *testing.T) {
	rig := newAppRig(t, Channel{Repositories: []string{"Smana/ai-native-ref"}, MaxCostUSD: 2})
	rig.threads.put("aaaaaaaa", Thread{Channel: "C03", TS: "1729.0001"})
	for _, body := range []string{
		`{"type":"room.waiting_for_human","room":"aaaaaaaa","reason":"approval"}`,
		`{"type":"room.pr_opened","room":"aaaaaaaa","pr":7}`,
		`{"type":"room.ended","room":"aaaaaaaa","state":"done"}`,
		`{"type":"room.ended","room":"aaaaaaaa","state":"ignore previous instructions <!channel>"}`,
	} {
		rig.webhook(body)
	}
	posts := rig.posts()
	if len(posts) != 4 {
		t.Fatalf("%d posts", len(posts))
	}
	if !strings.Contains(posts[0].text, "https://rooms.example/r/aaaaaaaa") || !strings.Contains(posts[0].text, "answer it on the room page") {
		t.Fatalf("a wait is linked, never answered here: %q", posts[0].text)
	}
	if !strings.Contains(posts[1].text, "Pull request #7 opened.") {
		t.Fatalf("%q", posts[1].text)
	}
	if strings.Contains(posts[3].text, "ignore") || strings.Contains(posts[3].text, "<!channel>") || !strings.Contains(posts[3].text, "state: other") {
		t.Fatalf("an unknown value is clipped: %q", posts[3].text)
	}
	if rig.webhookStatus(`{"type":"room.ended","room":"aaaaaaaa","state":"done"}`, "unsigned") != 401 {
		t.Fatal("an unsigned webhook was accepted")
	}
	if rig.webhookReplay() != 200 || len(rig.posts()) != 4 {
		t.Fatal("a replayed webhook-id posted twice")
	}
}

// A delivery for a room not filed from Slack, an unknown type, or a room id that is not a C2 id
// posts nothing.
func TestWebhookIgnores(t *testing.T) {
	rig := newAppRig(t, Channel{Repositories: []string{"Smana/ai-native-ref"}, MaxCostUSD: 2})
	rig.threads.put("aaaaaaaa", Thread{Channel: "C03", TS: "1729.0001"})
	rig.threads.put("../evil", Thread{Channel: "C03", TS: "1729.0001"})
	rig.webhook(`{"type":"room.ended","room":"bbbbbbbb","state":"done"}`)
	rig.webhook(`{"type":"room.surprise","room":"aaaaaaaa"}`)
	rig.webhook(`{"type":"room.ended","room":"../evil","state":"done"}`)
	rig.webhook(`{"type":"room.run_ended","room":"aaaaaaaa"}`) // a type with no template
	if n := len(rig.posts()); n != 0 {
		t.Fatalf("%d posts", n)
	}
}

// Invariant 8: nothing in a thread approves, steers or stops; the App reads no reply.
func TestSlackNeverDecides(t *testing.T) {
	rig := newAppRig(t, Channel{Repositories: []string{"Smana/ai-native-ref"}, MaxCostUSD: 2})
	rig.threads.put("aaaaaaaa", Thread{Channel: "C03", TS: "1729.0001"})
	for _, text := range []string{"approve", "/factory retry", "<@U0BOT> approve aaaaaaaa", "stop"} {
		rig.message("C03", "1729.0001", "U02DEF", text)
	}
	if calls := rig.broker.all(); len(calls) != 0 {
		t.Fatalf("a thread message reached the broker: %+v", calls)
	}
}

// ---- the rig ----

func draftPayload(repo, title, body string) DraftPayload {
	return DraftPayload{Repo: repo, Title: title, Body: body, SHA256: draftHash(repo, title, body)}
}

type post struct {
	ts, channel, thread, text string
	meta                      *Metadata
}

type fakeSlack struct {
	mu        sync.Mutex
	posts     []post
	msgs      map[string]Message
	failPosts int // the next n Posts fail
	threadErr error
}

func (f *fakeSlack) Post(_ context.Context, channel, threadTS, text string, meta *Metadata) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPosts > 0 {
		f.failPosts--
		return "", errors.New("slack down")
	}
	ts := fmt.Sprintf("1729.%04d", 1000+len(f.posts))
	f.posts = append(f.posts, post{ts: ts, channel: channel, thread: threadTS, text: text, meta: meta})
	f.msgs[ts] = Message{TS: ts, ThreadTS: threadTS, User: "U0BOT", Text: text, Metadata: meta}
	return ts, nil
}

func (f *fakeSlack) Message(_ context.Context, _, ts string) (Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.msgs[ts]
	if !ok {
		return Message{}, errors.New("message_not_found")
	}
	return m, nil
}

func (f *fakeSlack) Thread(_ context.Context, _, threadTS string) ([]Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.threadErr != nil {
		return nil, f.threadErr
	}
	var out []Message
	for _, m := range f.msgs {
		if m.TS == threadTS || m.ThreadTS == threadTS {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	return out, nil
}

func (f *fakeSlack) seed(m Message) { f.mu.Lock(); f.msgs[m.TS] = m; f.mu.Unlock() }

type stubBroker struct {
	mu       sync.Mutex
	calls    []any
	accepted []RunCreate
	refused  []RunCreate
	deny     map[string]string
	keys     map[string]bool
	draftErr error
	room     string // run_create's room, "aaaaaaaa" when empty
}

func (b *stubBroker) refuse(onBehalfOf, text string) {
	b.mu.Lock()
	b.deny[onBehalfOf] = text
	b.mu.Unlock()
}

func (b *stubBroker) IssueDraft(_ context.Context, d Draft) (Draft, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, d)
	if b.draftErr != nil {
		return Draft{}, b.draftErr
	}
	return d, nil
}

func (b *stubBroker) RunCreate(_ context.Context, c RunCreate) (RunResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, c)
	if text, ok := b.deny[c.OnBehalfOf]; ok {
		b.refused = append(b.refused, c)
		return RunResult{}, Refused(text)
	}
	b.accepted = append(b.accepted, c)
	dup := b.keys[c.Key]
	b.keys[c.Key] = true
	room := b.room
	if room == "" {
		room = "aaaaaaaa"
	}
	return RunResult{Room: room, Issue: 42, Duplicate: dup}, nil
}

func (b *stubBroker) newIssues() int { b.mu.Lock(); defer b.mu.Unlock(); return len(b.keys) }
func (b *stubBroker) created() int   { b.mu.Lock(); defer b.mu.Unlock(); return len(b.accepted) }
func (b *stubBroker) runCreates() []RunCreate {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]RunCreate(nil), b.accepted...)
}
func (b *stubBroker) refusedCalls() []RunCreate {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]RunCreate(nil), b.refused...)
}
func (b *stubBroker) all() []any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]any(nil), b.calls...)
}

type fakeThreads struct {
	mu      sync.Mutex
	m       map[string]Thread
	putFail int // the next n Puts fail
	puts    int
}

func (f *fakeThreads) Put(_ context.Context, room string, t Thread) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	if f.putFail > 0 {
		f.putFail--
		return errors.New("conflict")
	}
	f.m[room] = t
	return nil
}

func (f *fakeThreads) Get(_ context.Context, room string) (Thread, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.m[room]
	return t, ok, nil
}

func (f *fakeThreads) get(room string) (Thread, bool) {
	t, ok, _ := f.Get(context.Background(), room)
	return t, ok
}
func (f *fakeThreads) put(room string, t Thread) { _ = f.Put(context.Background(), room, t) }

type fakeMetrics struct {
	mu sync.Mutex
	n  map[string]int
}

func (m *fakeMetrics) Request(_ context.Context, route, outcome string) {
	m.mu.Lock()
	m.n[route+"/"+outcome]++
	m.mu.Unlock()
}

func (m *fakeMetrics) count(route, outcome string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.n[route+"/"+outcome]
}

type appRig struct {
	t       *testing.T
	app     *App
	slack   *fakeSlack
	broker  *stubBroker
	threads *fakeThreads
	metrics *fakeMetrics
	key     []byte
	now     time.Time
	seq     int
	lastWH  struct {
		body string
		h    http.Header
	}
}

const rigSecret = "slack-signing-secret"

func newAppRig(t *testing.T, ch Channel) *appRig {
	t.Helper()
	r := &appRig{t: t, now: time.Unix(1_790_000_000, 0), key: []byte("0123456789abcdef0123456789abcdef"),
		slack:   &fakeSlack{msgs: map[string]Message{"1729.0001": {TS: "1729.0001", User: "U02DEF", Text: "a human"}}},
		broker:  &stubBroker{deny: map[string]string{}, keys: map[string]bool{}},
		threads: &fakeThreads{m: map[string]Thread{}}, metrics: &fakeMetrics{n: map[string]int{}}}
	r.app = &App{Slack: r.slack, Broker: r.broker, Threads: r.threads,
		Verifier:    Verifier{Secret: func() string { return rigSecret }, Now: func() time.Time { return r.now }},
		WebhookKeys: func() ([][]byte, error) { return [][]byte{r.key}, nil },
		TeamID:      "T01ABC", BotUserID: "U0BOT", RoomsURL: "https://rooms.example", Channels: map[string]Channel{"C03": ch},
		Async: func(f func()) { f() }, Now: func() time.Time { return r.now }, Metrics: r.metrics}
	return r
}

func (r *appRig) eventBody(id, event string) string {
	return r.eventBodyTeam("T01ABC", id, event)
}

func (r *appRig) eventBodyTeam(team, id, event string) string {
	return `{"type":"event_callback","team_id":"` + team + `","event_id":"` + id + `","event":` + event + `}`
}

func (r *appRig) postEvents(body string, signed bool) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/slack/events", strings.NewReader(body))
	if signed {
		ts := strconv.FormatInt(r.now.Unix(), 10)
		req.Header.Set("X-Slack-Request-Timestamp", ts)
		req.Header.Set("X-Slack-Signature", slackSign(rigSecret, ts, body))
	}
	w := httptest.NewRecorder()
	r.app.Events().ServeHTTP(w, req)
	return w
}

func (r *appRig) event(event string) {
	r.t.Helper()
	r.seq++
	if st := r.postEvents(r.eventBody("Ev"+strconv.Itoa(r.seq), event), true).Code; st != 200 {
		r.t.Fatalf("events: %d", st)
	}
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func (r *appRig) mention(channel, ts, text string) {
	r.t.Helper()
	r.event(`{"type":"app_mention","channel":"` + channel + `","user":"U02DEF","ts":"` + ts + `","text":` + jsonString(text) + `}`)
}

func (r *appRig) react(channel, itemTS, user, reaction string) {
	r.t.Helper()
	r.event(`{"type":"reaction_added","user":"` + user + `","reaction":"` + reaction + `","item":{"type":"message","channel":"` + channel + `","ts":"` + itemTS + `"}}`)
}

func (r *appRig) message(channel, threadTS, user, text string) {
	r.t.Helper()
	r.event(`{"type":"message","channel":"` + channel + `","user":"` + user + `","thread_ts":"` + threadTS + `","ts":"1729.5000","text":` + jsonString(text) + `}`)
}

func (r *appRig) lastPost() post {
	r.t.Helper()
	p := r.posts()
	if len(p) == 0 {
		r.t.Fatal("nothing was posted")
	}
	return p[len(p)-1]
}

func (r *appRig) posts() []post {
	r.slack.mu.Lock()
	defer r.slack.mu.Unlock()
	return append([]post(nil), r.slack.posts...)
}

func (r *appRig) postWebhook(body string, h http.Header) int {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/webhooks/factory", strings.NewReader(body))
	for k, v := range h {
		req.Header[k] = v
	}
	w := httptest.NewRecorder()
	r.app.Webhooks().ServeHTTP(w, req)
	return w.Code
}

func (r *appRig) webhook(body string) {
	r.t.Helper()
	if st := r.webhookStatus(body, "signed"); st != 200 {
		r.t.Fatalf("webhook: %d", st)
	}
}

func (r *appRig) webhookStatus(body, mode string) int {
	r.seq++
	var h http.Header
	if mode == "signed" {
		h = webhook.Sign([][]byte{r.key}, "evt_"+strconv.Itoa(r.seq), r.now, []byte(body))
		r.lastWH.body, r.lastWH.h = body, h
	}
	return r.postWebhook(body, h)
}

func (r *appRig) webhookReplay() int { return r.postWebhook(r.lastWH.body, r.lastWH.h) }

func (r *appRig) eventTeam(team, event string) {
	r.t.Helper()
	r.seq++
	if st := r.postEvents(r.eventBodyTeam(team, "Ev"+strconv.Itoa(r.seq), event), true).Code; st != 200 {
		r.t.Fatalf("events: %d", st)
	}
}

const draftText = "<@U0BOT> Smana/ai-native-ref docs(agents): the link is dead\n## What is wrong\nx\n## What it should be\ny\n## Acceptance check\nz"

// M1: a thread-map failure after run_create must not leave the room unfollowed forever: the next
// ✅ is a duplicate and puts the thread again.
func TestDuplicateReactionPutsTheThread(t *testing.T) {
	rig := newAppRig(t, Channel{Repositories: []string{"Smana/ai-native-ref"}, MaxCostUSD: 2})
	rig.mention("C03", "1729.0001", draftText)
	draft := rig.lastPost()
	rig.threads.putFail = 1
	rig.react("C03", draft.ts, "U02DEF", "white_check_mark")
	if _, ok := rig.threads.get("aaaaaaaa"); ok {
		t.Fatal("the failed Put left an entry")
	}
	rig.react("C03", draft.ts, "U07GHI", "white_check_mark")
	if th, ok := rig.threads.get("aaaaaaaa"); !ok || th.TS != "1729.0001" {
		t.Fatalf("a duplicate did not restore the thread: %+v", th)
	}
}

// M2: an unreadable key file is the App's fault, not a bad signature: 503, counted no_key.
func TestWebhookKeyProblemIs503(t *testing.T) {
	rig := newAppRig(t, Channel{Repositories: []string{"Smana/ai-native-ref"}, MaxCostUSD: 2})
	rig.app.WebhookKeys = func() ([][]byte, error) { return nil, errors.New("no such file") }
	if st := rig.webhookStatus(`{"type":"room.run_started","room":"aaaaaaaa"}`, "signed"); st != 503 {
		t.Fatalf("%d", st)
	}
	if rig.metrics.count("webhooks", "no_key") != 1 || rig.metrics.count("webhooks", "bad_signature") != 0 {
		t.Fatal("outcome")
	}
}

// M4: one workspace. An event from another team (a Slack Connect peer) does nothing.
func TestOtherWorkspaceIsDropped(t *testing.T) {
	rig := newAppRig(t, Channel{Repositories: []string{"Smana/ai-native-ref"}, MaxCostUSD: 2})
	rig.mention("C03", "1729.0001", draftText)
	draft := rig.lastPost()
	n := len(rig.posts())
	rig.eventTeam("T09OTHER", `{"type":"app_mention","channel":"C03","user":"U02DEF","ts":"1729.0002","text":`+jsonString(draftText)+`}`)
	rig.eventTeam("T09OTHER", `{"type":"reaction_added","user":"U02DEF","reaction":"white_check_mark","item":{"type":"message","channel":"C03","ts":"`+draft.ts+`"}}`)
	if len(rig.posts()) != n || len(rig.broker.all()) != 1 { // the one IssueDraft of the first mention
		t.Fatalf("a foreign workspace's event was handled: %d posts, %d calls", len(rig.posts()), len(rig.broker.all()))
	}
}

// M5: a mention from a malformed user id, or any event with bot_id, is ignored.
func TestMentionSenderChecks(t *testing.T) {
	rig := newAppRig(t, Channel{Repositories: []string{"Smana/ai-native-ref"}, MaxCostUSD: 2})
	rig.event(`{"type":"app_mention","channel":"C03","user":"u<!channel>","ts":"1729.0001","text":` + jsonString(draftText) + `}`)
	rig.event(`{"type":"app_mention","channel":"C03","user":"U02DEF","bot_id":"B01","ts":"1729.0002","text":` + jsonString(draftText) + `}`)
	rig.event(`{"type":"reaction_added","user":"U02DEF","bot_id":"B01","reaction":"white_check_mark","item":{"type":"message","channel":"C03","ts":"1729.0001"}}`)
	if len(rig.posts()) != 0 || len(rig.broker.all()) != 0 {
		t.Fatal("handled")
	}
}

// M6: the thread never shows the package's internal error prefix.
func TestRefusalHidesInternalPrefix(t *testing.T) {
	rig := newAppRig(t, Channel{Repositories: []string{"Smana/ai-native-ref"}, MaxCostUSD: 2})
	rig.mention("C03", "1729.0001", "<@U0BOT>")
	if p := rig.lastPost(); !strings.HasPrefix(p.text, "Draft refused: ") || strings.Contains(p.text, "slackapp:") {
		t.Fatalf("%q", p.text)
	}
}

// M7: a task was filed under a room id that cannot be followed: say so, with no free text.
func TestUnfollowableRoomIsSaid(t *testing.T) {
	rig := newAppRig(t, Channel{Repositories: []string{"Smana/ai-native-ref"}, MaxCostUSD: 2})
	rig.broker.room = "../evil <!channel>"
	rig.mention("C03", "1729.0001", draftText)
	rig.react("C03", rig.lastPost().ts, "U02DEF", "white_check_mark")
	p := rig.lastPost().text
	if !strings.Contains(p, "filed") || !strings.Contains(p, "cannot be followed") || strings.Contains(p, "evil") || strings.Contains(p, "channel>") {
		t.Fatalf("%q", p)
	}
	if len(rig.threads.m) != 0 {
		t.Fatal("an invalid room was mapped")
	}
}

// M8: nothing Slack would act on survives into the draft post, in the title or the body.
func TestDraftPostIsInert(t *testing.T) {
	rig := newAppRig(t, Channel{Repositories: []string{"Smana/ai-native-ref"}, MaxCostUSD: 2})
	evil := "<!channel> <@U123> <https://x|y>"
	rig.mention("C03", "1729.0001", "<@U0BOT> Smana/ai-native-ref t "+evil+"\nbody "+evil+"\n```\n<!here>\n```")
	p := rig.lastPost()
	for _, bad := range []string{"<!channel>", "<@U123>", "<https://x|y>", "<!here>"} {
		if strings.Contains(p.text, bad) {
			t.Fatalf("%s survived: %q", bad, p.text)
		}
	}
	if strings.Contains(p.text, "```") {
		t.Fatalf("a fence from the body survived: %q", p.text)
	}
	if p.meta == nil || !strings.Contains(p.meta.EventPayload.Title, "<!channel>") {
		t.Fatal("the metadata must keep the draft as the broker returned it")
	}
}

// M8: a Slack failure is a 502, the id is forgotten, and the redelivery posts.
func TestWebhookRedeliveryAfterSlackFailure(t *testing.T) {
	rig := newAppRig(t, Channel{Repositories: []string{"Smana/ai-native-ref"}, MaxCostUSD: 2})
	rig.threads.put("aaaaaaaa", Thread{Channel: "C03", TS: "1729.0001"})
	rig.slack.failPosts = 1
	body := `{"type":"room.run_started","room":"aaaaaaaa"}`
	if st := rig.webhookStatus(body, "signed"); st != 502 {
		t.Fatalf("%d", st)
	}
	if st := rig.webhookReplay(); st != 200 || len(rig.posts()) != 1 {
		t.Fatalf("redelivery: %d, %d posts", st, len(rig.posts()))
	}
}

// M8: a delivery whose timestamp is outside the tolerance is refused by the App.
func TestWebhookStaleTimestamp(t *testing.T) {
	rig := newAppRig(t, Channel{Repositories: []string{"Smana/ai-native-ref"}, MaxCostUSD: 2})
	rig.threads.put("aaaaaaaa", Thread{Channel: "C03", TS: "1729.0001"})
	body := `{"type":"room.run_started","room":"aaaaaaaa"}`
	h := webhook.Sign([][]byte{rig.key}, "evt_stale", rig.now.Add(-10*time.Minute), []byte(body))
	if st := rig.postWebhook(body, h); st != 401 || len(rig.posts()) != 0 {
		t.Fatalf("%d", st)
	}
}

// A draft whose metadata would pass Slack's cap is not posted: the thread is told it is too long,
// without any of its text, and nothing is left to check-mark.
func TestOversizedDraftIsNotPosted(t *testing.T) {
	rig := newAppRig(t, Channel{Repositories: []string{"Smana/ai-native-ref"}, MaxCostUSD: 2})
	body := strings.Repeat("secret-body-text ", 1000)
	rig.mention("C03", "1729.0001", "<@U0BOT> Smana/ai-native-ref a title\n"+body)
	posts := rig.posts()
	if len(posts) != 1 {
		t.Fatalf("%d posts", len(posts))
	}
	p := posts[0]
	if p.meta != nil || !strings.Contains(p.text, "too long to file from Slack") || strings.Contains(p.text, "secret-body-text") || strings.Contains(p.text, "a title") {
		t.Fatalf("%+v", p)
	}
	if rig.metrics.count("events", "too_large") != 1 || rig.metrics.count("events", "drafted") != 0 {
		t.Fatal("outcome")
	}
}

// O14: a one-repository channel takes a title-only line 1; a many-repository channel's refusal
// shows an example.
func TestMentionOnAOneRepositoryChannel(t *testing.T) {
	rig := newAppRig(t, Channel{Repositories: []string{"Smana/ai-native-ref"}, MaxCostUSD: 2})
	rig.mention("C03", "1729.0001", "<@U0BOT> docs(agents): the link is dead\n## What is wrong\nx\n## What it should be\ny\n## Acceptance check\nz")
	c := rig.broker.all()
	if d, ok := c[0].(Draft); len(c) != 1 || !ok || d.Repo != "Smana/ai-native-ref" || d.Title != "docs(agents): the link is dead" {
		t.Fatalf("%+v %q", c, rig.lastPost().text)
	}
	two := newAppRig(t, Channel{Repositories: []string{"Smana/ai-native-ref", "Smana/cloud-native-ref"}, MaxCostUSD: 2})
	two.mention("C03", "1729.0001", "<@U0BOT> docs: a title without a repository\nbody")
	if p := two.lastPost(); !strings.HasPrefix(p.text, "Draft refused: ") || !strings.Contains(p.text, "Try: `@factory Smana/ai-native-ref docs: a title without a repository`") || len(two.broker.all()) != 0 {
		t.Fatalf("%q", p.text)
	}
}

// A one-line mention is a draft: the thread shows the composed issue the check mark will file.
func TestSingleLineMentionPostsTheComposedDraft(t *testing.T) {
	rig := newAppRig(t, Channel{Repositories: []string{"Smana/ai-native-ref"}, MaxCostUSD: 2})
	rig.mention("C03", "1729.0001", "<@U0BOT> docs: fix a dead link")
	p := rig.lastPost().text
	if !strings.HasPrefix(p, "Draft for ") || !strings.Contains(p, "\n\n*What is wrong*\ndocs: fix a dead link\n") || !strings.Contains(p, "*Acceptance check*\nNone given in Slack") || strings.Contains(p, "##") {
		t.Fatalf("%q", p)
	}
}

// ---- draft editing in the thread ----

func (r *appRig) reply(user, text string) {
	r.t.Helper()
	r.seq++
	r.event(`{"type":"message","channel":"C03","user":"` + user + `","thread_ts":"1729.0001","ts":"1729.9` + strconv.Itoa(100+r.seq) + `","text":` + jsonString(text) + `}`)
}

func editRig(t *testing.T) *appRig {
	t.Helper()
	return newAppRig(t, Channel{Repositories: []string{"Smana/ai-native-ref"}, MaxCostUSD: 2})
}

func (r *appRig) payload(p post) DraftPayload { return p.meta.EventPayload }

// Today's case: a one-line mention, then a reply that adds the acceptance check.
func TestReplyAddsTheAcceptanceCheck(t *testing.T) {
	rig := editRig(t)
	rig.mention("C03", "1729.0001", "<@U0BOT> docs: runbook 18 ends without a pointer")
	first := rig.lastPost()
	if !strings.Contains(first.text, "*Acceptance check*\n"+noAcceptance) {
		t.Fatalf("%q", first.text)
	}
	rig.reply("U02DEF", "Acceptance: the runbook ends with a See also line to runbook 20")
	if len(rig.posts()) != 2 {
		t.Fatalf("%d posts", len(rig.posts()))
	}
	second := rig.lastPost()
	p := rig.payload(second)
	if !strings.Contains(p.Body, "## Acceptance check\nthe runbook ends with a See also line to runbook 20") ||
		!strings.Contains(p.Body, "## What is wrong\ndocs: runbook 18 ends without a pointer") ||
		p.Author != "U02DEF" || p.Rev != 2 || p.SHA256 != draftHash(p.Repo, p.Title, p.Body) {
		t.Fatalf("%+v", p)
	}
	if rig.metrics.count("events", "edited") != 1 {
		t.Fatal("the edit went uncounted")
	}
}

func TestReplyKinds(t *testing.T) {
	for _, tc := range []struct{ name, reply, want, wantTitle string }{
		{"title", "Title: runbook 18 gets a see also", "", "docs: runbook 18 gets a see also"},
		{"title, doc word in the body", "Title: fix the thing", "", "docs: fix the thing"},
		{"should", "Should: end with a pointer", "## What it should be\nend with a pointer\n", ""},
		{"expected", "Expected: a pointer", "## What it should be\na pointer\n", ""},
		{"done when", "Done when: line one\nline two", "## Acceptance check\nline one\nline two", ""},
		{"check", "Check: it ends with a link", "## Acceptance check\nit ends with a link", ""},
		{"free text", "also the table is wrong", "## What is wrong\nalso the table is wrong\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := editRig(t)
			rig.mention("C03", "1729.0001", "<@U0BOT> docs: runbook 18 ends badly")
			rig.reply("U02DEF", tc.reply)
			p := rig.payload(rig.lastPost())
			if rig.payload(rig.posts()[0]).SHA256 == p.SHA256 {
				t.Fatal("nothing changed")
			}
			if tc.want != "" && !strings.Contains(p.Body, tc.want) {
				t.Fatalf("%q", p.Body)
			}
			if tc.wantTitle != "" && p.Title != tc.wantTitle {
				t.Fatalf("%q", p.Title)
			}
		})
	}
}

func TestReplyAppendsToWhatIsWrong(t *testing.T) {
	rig := editRig(t)
	rig.mention("C03", "1729.0001", "<@U0BOT> docs: runbook 18\nthe pointer is missing")
	rig.reply("U02DEF", "and the table is stale")
	if !strings.Contains(rig.payload(rig.lastPost()).Body, "## What is wrong\nthe pointer is missing\n\nand the table is stale\n") {
		t.Fatalf("%q", rig.payload(rig.lastPost()).Body)
	}
}

func TestReplyIgnored(t *testing.T) {
	rig := editRig(t)
	rig.mention("C03", "1729.0001", "<@U0BOT> docs: runbook 18 ends badly")
	rig.reply("U07GHI", "Acceptance: someone else's") // not the author
	rig.reply("U02DEF", "   ")                        // nothing to apply
	rig.reply("U0BOT", "Acceptance: the bot's own")
	rig.event(`{"type":"message","subtype":"message_changed","channel":"C03","user":"U02DEF","thread_ts":"1729.0001","ts":"1729.9","text":"Acceptance: x"}`)
	rig.event(`{"type":"message","channel":"C03","user":"U02DEF","ts":"1729.7","text":"Acceptance: top level"}`)
	rig.event(`{"type":"message","channel":"C99","user":"U02DEF","thread_ts":"1729.0001","ts":"1729.8","text":"Acceptance: other channel"}`)
	rig.reply("U02DEF", "<@U0BOT> a mention in a reply is the mention flow's")
	if n := len(rig.posts()); n != 1 { // the draft alone: no reply, no note
		t.Fatalf("%d posts: %+v", n, rig.posts())
	}
}

func TestReplyAfterFilingIsIgnored(t *testing.T) {
	rig := editRig(t)
	rig.mention("C03", "1729.0001", "<@U0BOT> docs: runbook 18 ends badly")
	draft := rig.lastPost()
	rig.react("C03", draft.ts, "U02DEF", "white_check_mark")
	if rig.broker.created() != 1 || rig.lastPost().meta == nil || rig.lastPost().meta.EventType != "factory_filed" {
		t.Fatalf("%+v", rig.lastPost())
	}
	before := len(rig.posts())
	rig.reply("U02DEF", "Acceptance: too late")
	if len(rig.posts()) != before {
		t.Fatal("a filed draft was edited")
	}
	rig.react("C03", draft.ts, "U07GHI", "white_check_mark") // a second ✅ on the filed draft is still a duplicate
	if !strings.Contains(rig.lastPost().text, "Already filed") {
		t.Fatalf("%q", rig.lastPost().text)
	}
}

func TestReactionOnASupersededDraftIsIgnored(t *testing.T) {
	rig := editRig(t)
	rig.mention("C03", "1729.0001", "<@U0BOT> docs: runbook 18 ends badly")
	old := rig.lastPost()
	rig.reply("U02DEF", "Acceptance: a see also line")
	latest := rig.lastPost()
	rig.react("C03", old.ts, "U02DEF", "white_check_mark")
	if rig.broker.created() != 0 || !strings.Contains(rig.lastPost().text, "latest draft") {
		t.Fatalf("a superseded draft was filed or the note is missing: %q", rig.lastPost().text)
	}
	rig.react("C03", latest.ts, "U02DEF", "white_check_mark")
	calls := rig.broker.runCreates()
	if len(calls) != 1 || !strings.Contains(calls[0].Body, "a see also line") {
		t.Fatalf("%+v", calls)
	}
}

func TestReplyRevisionCap(t *testing.T) {
	rig := editRig(t)
	rig.mention("C03", "1729.0001", "<@U0BOT> docs: runbook 18 ends badly")
	for i := range maxRevisions + 3 {
		rig.reply("U02DEF", fmt.Sprintf("more %d", i))
	}
	drafts := 0
	for _, p := range rig.posts() {
		if p.meta != nil {
			drafts++
		}
	}
	if drafts != maxRevisions {
		t.Fatalf("%d drafts", drafts)
	}
	if !strings.Contains(rig.lastPost().text, "most it can be") {
		t.Fatalf("%q", rig.lastPost().text)
	}
}

func TestReplyIsInertAndRefusalsAreSaid(t *testing.T) {
	rig := editRig(t)
	rig.mention("C03", "1729.0001", "<@U0BOT> docs: runbook 18 ends badly")
	rig.reply("U02DEF", "Acceptance: ping <!channel> and <@U07GHI> ```x```")
	txt := rig.lastPost().text
	if strings.Contains(txt, "<!channel>") || strings.Contains(txt, "<@U07GHI>") || !strings.Contains(txt, "&lt;!channel&gt;") {
		t.Fatalf("%q", txt)
	}
	rig.broker.draftErr = Refused("rejected: bad")
	n := len(rig.posts())
	rig.reply("U02DEF", "Should: x")
	if len(rig.posts()) != n+1 || !strings.HasPrefix(rig.lastPost().text, "Change refused: rejected: bad") || rig.lastPost().meta != nil {
		t.Fatalf("%+v", rig.lastPost())
	}
}

func TestSlackDraftIsMrkdwn(t *testing.T) {
	d := Draft{Repo: "Smana/ai-native-ref", Title: "docs: fix", Body: composeBody("docs: fix", "wrong *thing*\nShould: right\nAcceptance: done")}
	got := slackDraft(d)
	for _, want := range []string{
		"Draft for Smana/ai-native-ref: *docs: fix*\n\n*What is wrong*\nwrong *thing*\n\n",
		"*What it should be*\nright\n\n", "*Acceptance check*\ndone\n\n", "Reply in this thread", ":white_check_mark:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "##") {
		t.Fatalf("a raw heading in Slack: %q", got)
	}
}

// One issue per thread: a failed "Filed" post, an edit and a check mark on the newer draft do not file twice.
func TestRefilingAfterAFailedFiledPostIsADuplicate(t *testing.T) {
	rig := editRig(t)
	rig.mention("C03", "1729.0001", "<@U0BOT> docs: runbook 18 ends badly")
	d1 := rig.lastPost()
	rig.slack.failPosts = 1 // the "Filed" line is lost
	rig.react("C03", d1.ts, "U02DEF", "white_check_mark")
	rig.reply("U02DEF", "Acceptance: a see also line")
	rig.react("C03", rig.lastPost().ts, "U02DEF", "white_check_mark")
	calls := rig.broker.runCreates()
	if len(calls) != 2 || calls[0].Key != calls[1].Key || rig.broker.created() != 2 || !strings.Contains(rig.lastPost().text, "Already filed") {
		t.Fatalf("%+v", calls)
	}
	if n := rig.broker.newIssues(); n != 1 {
		t.Fatalf("%d issues", n)
	}
}

func TestLongThreadFailsClosed(t *testing.T) {
	rig := editRig(t)
	rig.mention("C03", "1729.0001", "<@U0BOT> docs: runbook 18 ends badly")
	d := rig.lastPost()
	rig.slack.threadErr = ErrThreadTooLong
	n := len(rig.posts())
	rig.reply("U02DEF", "Acceptance: x")
	rig.react("C03", d.ts, "U02DEF", "white_check_mark")
	if rig.broker.created() != 0 || len(rig.posts()) != n+2 || !strings.Contains(rig.lastPost().text, "too long") {
		t.Fatalf("%d %q", rig.broker.created(), rig.lastPost().text)
	}
}

// ---- a check mark typed as a reply ----

func TestCheckMarkReplyFilesTheLatestDraft(t *testing.T) {
	rig := editRig(t)
	rig.mention("C03", "1729.0001", "<@U0BOT> docs: runbook 18 ends badly")
	rig.reply("U02DEF", "Acceptance: a see also line")
	n := len(rig.posts())
	rig.reply("U07GHI", " :white_check_mark: ")
	calls := rig.broker.runCreates()
	if len(calls) != 1 || calls[0].OnBehalfOf != "slack:T01ABC/U07GHI" || calls[0].Key != "T01ABC/C03/1729.0001" ||
		calls[0].Ref != "slack/T01ABC/C03/1729.0001" || !strings.Contains(calls[0].Body, "a see also line") {
		t.Fatalf("%+v", calls)
	}
	if len(rig.posts()) != n+1 || rig.lastPost().meta == nil || rig.lastPost().meta.EventType != "factory_filed" ||
		!strings.Contains(rig.lastPost().text, "Filed #42") {
		t.Fatalf("%+v", rig.lastPost())
	}
	for _, text := range []string{"✅", ":heavy_check_mark:"} { // on the filed thread: a duplicate, one issue
		rig.reply("U02DEF", text)
		if !strings.Contains(rig.lastPost().text, "Already filed") {
			t.Fatalf("%q", rig.lastPost().text)
		}
	}
	if rig.broker.newIssues() != 1 {
		t.Fatal("a second issue was filed")
	}
}

func TestCheckMarkReplyByANonMaintainerIsRefused(t *testing.T) {
	rig := editRig(t)
	rig.mention("C03", "1729.0001", "<@U0BOT> docs: runbook 18 ends badly")
	rig.broker.refuse("slack:T01ABC/U09XYZ", "rejected: not_linked")
	n := len(rig.posts())
	rig.reply("U09XYZ", "✅")
	if rig.broker.newIssues() != 0 || len(rig.posts()) != n+1 || rig.lastPost().meta != nil ||
		!strings.Contains(rig.lastPost().text, "A maintainer reacts :white_check_mark: to file it") {
		t.Fatalf("%+v", rig.lastPost())
	}
}

func TestCheckMarkReplyIsNeverDraftText(t *testing.T) {
	rig := editRig(t)
	rig.mention("C03", "1729.0001", "<@U0BOT> docs: runbook 18 ends badly")
	rig.broker.refuse("slack:T01ABC/U02DEF", "rejected: not_linked")
	rig.reply("U02DEF", ":white_check_mark:")
	for _, p := range rig.posts() {
		if p.meta != nil && p.meta.EventPayload.Rev > 1 {
			t.Fatalf("the check mark was appended: %+v", p.meta.EventPayload)
		}
	}
	// no draft in the thread: nothing at all
	rig2 := editRig(t)
	rig2.reply("U02DEF", "✅")
	if len(rig2.posts()) != 0 || len(rig2.broker.all()) != 0 {
		t.Fatal("a check mark in a thread without a draft did something")
	}
}
