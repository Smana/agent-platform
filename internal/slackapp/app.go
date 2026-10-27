// SPDX-License-Identifier: Apache-2.0

package slackapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Smana/agent-platform/internal/webhook"
)

const (
	draftEventType = "factory_draft"
	handleTimeout  = 30 * time.Second
	// maxMetadata bounds the draft's metadata JSON, well under Slack's message-metadata cap.
	maxMetadata    = 8 << 10
	maxWebhookBody = 64 << 10
	seenCap        = 4096
	maxShown       = 300
)

// The ids that reach a ref, a key or a URL are checked against Slack's and the factory's shapes
// first; anything else is ignored rather than escaped.
var (
	channelRE = regexp.MustCompile(`^[CG][A-Z0-9]{2,20}$`)
	teamRE    = regexp.MustCompile(`^T[A-Z0-9]{2,20}$`)
	userRE    = regexp.MustCompile(`^[UW][A-Z0-9]{2,20}$`)
	tsRE      = regexp.MustCompile(`^[0-9]{1,12}\.[0-9]{1,8}$`)
	roomRE    = regexp.MustCompile(`^[a-z2-7]{8}$`)
)

// BrokerAPI is what the flows call on the broker; Broker has it, and the test fakes it.
type BrokerAPI interface {
	IssueDraft(ctx context.Context, d Draft) (Draft, error)
	RunCreate(ctx context.Context, c RunCreate) (RunResult, error)
}

// Recorder counts requests for factory_slack_requests_total{route, outcome}.
type Recorder interface {
	Request(ctx context.Context, route, outcome string)
}

// App is the flows: a mention drafts, a maintainer's check mark files, the broker's webhooks tell
// the thread. It reads no thread reply and answers no approval (invariant 8).
type App struct {
	Slack       Slack
	Broker      BrokerAPI
	Threads     Threads
	Verifier    Verifier
	WebhookKeys func() ([][]byte, error)
	TeamID      string
	BotUserID   string
	RoomsURL    string
	Channels    map[string]Channel
	Async       func(func()) // runs an event's handling after the 200; nil means go f()
	Now         func() time.Time
	Metrics     Recorder
	Logger      *slog.Logger

	events, deliveries seen // event_id and webhook-id
}

// seen is a bounded set of ids: Slack's event_id and a delivery's webhook-id (R26).
type seen struct {
	mu    sync.Mutex
	order []string
	set   map[string]struct{}
}

// add reports whether id is new.
func (s *seen) add(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.set[id]; ok {
		return false
	}
	if s.set == nil {
		s.set = map[string]struct{}{}
	}
	if len(s.order) >= seenCap {
		delete(s.set, s.order[0])
		s.order = s.order[1:]
	}
	s.set[id] = struct{}{}
	s.order = append(s.order, id)
	return true
}

func (s *seen) forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.set, id)
	s.order = slices.DeleteFunc(s.order, func(o string) bool { return o == id })
}

func draftHash(repo, title, body string) string {
	sum := sha256.Sum256([]byte(repo + "\x00" + title + "\x00" + body))
	return hex.EncodeToString(sum[:])
}

func (a *App) count(ctx context.Context, route, outcome string) {
	if a.Metrics != nil {
		a.Metrics.Request(ctx, route, outcome)
	}
}

func (a *App) log() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.New(slog.DiscardHandler)
}

func (a *App) run(f func()) {
	if a.Async != nil {
		a.Async(f)
		return
	}
	go f()
}

// slackText is user text made inert in a Slack message: Slack's three escapes, so a <!channel>,
// a <@user> or a link in a draft never fires, and no fence to close a code block early.
func slackText(s string) string {
	s = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
	return strings.ReplaceAll(s, "```", "'''")
}

// shown is a refusal as the thread shows it: inert and short.
func shown(s string) string {
	s = strings.TrimPrefix(s, "slackapp: ") // the package's internal prefix is not for people
	if r := []rune(s); len(r) > maxShown {
		s = string(r[:maxShown]) + "…"
	}
	return slackText(s)
}

type slackEnvelope struct {
	Type      string          `json:"type"`
	Challenge string          `json:"challenge"`
	TeamID    string          `json:"team_id"`
	EventID   string          `json:"event_id"`
	Event     json.RawMessage `json:"event"`
}

type slackEvent struct {
	Type     string `json:"type"`
	Channel  string `json:"channel"`
	User     string `json:"user"`
	BotID    string `json:"bot_id"`
	Text     string `json:"text"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts"`
	Reaction string `json:"reaction"`
	Item     struct {
		Type    string `json:"type"`
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	} `json:"item"`
}

// Events is the events listener's handler: the signature first, nothing read before it.
func (a *App) Events() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		body, err := a.Verifier.Body(r)
		if err != nil {
			a.count(ctx, "events", "bad_signature")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var env slackEnvelope
		if json.Unmarshal(body, &env) != nil {
			a.count(ctx, "events", "bad_request")
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		switch env.Type {
		case "url_verification":
			a.count(ctx, "events", "ok")
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, env.Challenge)
		case "event_callback":
			if env.EventID == "" || !a.events.add(env.EventID) {
				a.count(ctx, "events", "duplicate")
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusOK)
			a.run(func() {
				hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), handleTimeout)
				defer cancel()
				a.count(hctx, "events", a.handle(hctx, env))
			})
		default:
			a.count(ctx, "events", "ignored")
			w.WriteHeader(http.StatusOK)
		}
	})
}

// handle runs one event and names its outcome for the counter.
func (a *App) handle(ctx context.Context, env slackEnvelope) string {
	var ev slackEvent
	// One workspace: a Slack Connect peer's events carry its own team_id. A bot's event (bot_id)
	// is never a person's mention or check mark.
	if json.Unmarshal(env.Event, &ev) != nil || !teamRE.MatchString(env.TeamID) || env.TeamID != a.TeamID || ev.BotID != "" {
		return "ignored"
	}
	switch {
	case ev.Type == "app_mention":
		return a.mention(ctx, ev)
	case ev.Type == "reaction_added" && ev.Reaction == "white_check_mark" && ev.Item.Type == "message":
		return a.reaction(ctx, env.TeamID, ev)
	}
	return "ignored" // a thread message is never read for a command (invariant 8)
}

func (a *App) mention(ctx context.Context, ev slackEvent) string {
	ch, ok := a.Channels[ev.Channel]
	thread := ev.ThreadTS
	if thread == "" {
		thread = ev.TS
	}
	if !ok || ev.User == a.BotUserID || !userRE.MatchString(ev.User) || !tsRE.MatchString(thread) {
		return "ignored"
	}
	refuse := func(why string) string {
		if _, err := a.Slack.Post(ctx, ev.Channel, thread, "Draft refused: "+shown(why), nil); err != nil {
			a.log().Warn("slack post failed", "channel", ev.Channel, "err", err)
			return "error"
		}
		return "refused"
	}
	only := ""
	if len(ch.Repositories) == 1 {
		only = ch.Repositories[0]
	}
	d, err := ParseMention(ev.Text, a.BotUserID, only)
	if err != nil {
		return refuse(err.Error())
	}
	if !slices.Contains(ch.Repositories, d.Repo) {
		return refuse("this channel does not serve that repository")
	}
	d, err = a.Broker.IssueDraft(ctx, d)
	if err != nil {
		if r, ok := err.(Refused); ok { //nolint:errorlint // Refused is returned bare, never wrapped
			return refuse(string(r))
		}
		a.log().Warn("issue_draft failed", "err", err)
		return refuse("the draft could not be checked, try again")
	}
	text := fmt.Sprintf("Draft for %s: *%s*; a maintainer reacts :white_check_mark: to file it and start the work.\n```%s```",
		slackText(d.Repo), slackText(d.Title), slackText(d.Body))
	meta := &Metadata{EventType: draftEventType, EventPayload: DraftPayload{Repo: d.Repo, Title: d.Title, Body: d.Body,
		SHA256: draftHash(d.Repo, d.Title, d.Body)}}
	if mj, err := json.Marshal(meta); err != nil || len(mj) > maxMetadata {
		// Slack would refuse the metadata: say so, with none of the draft's text.
		if _, err := a.Slack.Post(ctx, ev.Channel, thread, "Draft not posted: it is too long to file from Slack; file it from the web UI.", nil); err != nil {
			a.log().Warn("slack post failed", "channel", ev.Channel, "err", err)
			return "error"
		}
		return "too_large"
	}
	if _, err := a.Slack.Post(ctx, ev.Channel, thread, text, meta); err != nil {
		a.log().Warn("slack post failed", "channel", ev.Channel, "err", err)
		return "error"
	}
	return "drafted"
}

func (a *App) reaction(ctx context.Context, team string, ev slackEvent) string {
	ch, ok := a.Channels[ev.Item.Channel]
	if !ok || !channelRE.MatchString(ev.Item.Channel) || !userRE.MatchString(ev.User) || !tsRE.MatchString(ev.Item.TS) {
		return "ignored"
	}
	msg, err := a.Slack.Message(ctx, ev.Item.Channel, ev.Item.TS)
	if err != nil {
		a.log().Warn("slack message fetch failed", "channel", ev.Item.Channel, "err", err)
		return "error"
	}
	// Only the App's own draft counts: posted by its bot user, carrying a draft whose hash holds (R23).
	m := msg.Metadata
	if msg.User == "" || msg.User != a.BotUserID || m == nil || m.EventType != draftEventType {
		return "ignored"
	}
	p := m.EventPayload
	if p.SHA256 != draftHash(p.Repo, p.Title, p.Body) || !slices.Contains(ch.Repositories, p.Repo) {
		return "ignored"
	}
	thread := msg.ThreadTS
	if thread == "" {
		thread = msg.TS
	}
	if !tsRE.MatchString(thread) {
		return "ignored"
	}
	say := func(text string) { // the verdict goes to the thread; a failure to say it is logged, not retried
		if _, err := a.Slack.Post(ctx, ev.Item.Channel, thread, text, nil); err != nil {
			a.log().Warn("slack post failed", "channel", ev.Item.Channel, "err", err)
		}
	}
	res, err := a.Broker.RunCreate(ctx, RunCreate{
		Draft:      Draft{Repo: p.Repo, Title: p.Title, Body: p.Body},
		MaxCostUSD: ch.MaxCostUSD,
		OnBehalfOf: "slack:" + team + "/" + ev.User,
		Ref:        "slack/" + team + "/" + ev.Item.Channel + "/" + thread,
		Key:        team + "/" + ev.Item.Channel + "/" + ev.Item.TS,
	})
	if err != nil {
		if r, ok := err.(Refused); ok { //nolint:errorlint // Refused is returned bare, never wrapped
			say("Not filed: " + shown(string(r)))
			return "refused"
		}
		a.log().Warn("run_create failed", "err", err)
		say("Not filed: the factory could not be reached, react again to retry.")
		return "error"
	}
	if !roomRE.MatchString(res.Room) {
		a.log().Warn("run_create answered an unusable room id")
		say(fmt.Sprintf("The task is filed (#%d, for <@%s>), but its room cannot be followed here.", res.Issue, ev.User))
		return "error"
	}
	url := strings.TrimSuffix(a.RoomsURL, "/") + "/r/" + res.Room
	// Put on a duplicate too (idempotent): a Put that failed the first time is retried by the next ✅.
	if err := a.Threads.Put(ctx, res.Room, Thread{Channel: ev.Item.Channel, TS: thread, At: a.Now()}); err != nil {
		a.log().Warn("thread map put failed", "room", res.Room, "err", err)
	}
	if res.Duplicate {
		say("Already filed: " + url)
		return "duplicate"
	}
	say(fmt.Sprintf("Filed #%d for <@%s>; the factory starts it. Room: %s", res.Issue, ev.User, url))
	return "filed"
}

// Webhooks is the webhooks listener's handler: the delivery is verified with the broker's keys
// (read at use) before it is decoded. It answers once the thread is told, so a Slack failure is a
// 502 and the broker's at-least-once retry brings the event back.
func (a *App) Webhooks() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody+1))
		keys, kerr := a.WebhookKeys()
		if kerr != nil {
			// Our own key problem, not a bad signature: a 503 so it is told apart in the metrics. It is
			// not a hold: the broker retries a non-2xx after 10s, 1m and 5m, then counts a failure, and
			// three given-up deliveries disable the subscriber until its generation is bumped.
			a.log().Error("webhook keys unreadable")
			a.count(ctx, "webhooks", "no_key")
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if err != nil || len(body) > maxWebhookBody {
			a.count(ctx, "webhooks", "bad_signature")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		id, err := webhook.Verify(keys, r.Header, body, a.Now())
		if err != nil {
			a.count(ctx, "webhooks", "bad_signature")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Known race (M3): two concurrent redeliveries of one id can both pass add before either
		// finishes; the worst case is one duplicate thread line, and a failure forgets the id.
		if !a.deliveries.add(id) {
			a.count(ctx, "webhooks", "duplicate")
			w.WriteHeader(http.StatusOK)
			return
		}
		hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), handleTimeout)
		defer cancel()
		outcome := a.deliver(hctx, body)
		if outcome == "error" {
			a.deliveries.forget(id)
			a.count(ctx, "webhooks", outcome)
			http.Error(w, "try again", http.StatusBadGateway)
			return
		}
		a.count(ctx, "webhooks", outcome)
		w.WriteHeader(http.StatusOK)
	})
}

func (a *App) deliver(ctx context.Context, body []byte) string {
	var e webhook.Event
	if json.Unmarshal(body, &e) != nil || !slices.Contains(webhook.Types(), e.Type) || !roomRE.MatchString(e.Room) {
		return "ignored"
	}
	line, ok := a.line(e)
	if !ok {
		return "ignored"
	}
	th, found, err := a.Threads.Get(ctx, e.Room)
	if err != nil {
		a.log().Warn("thread map get failed", "room", e.Room, "err", err)
		return "error"
	}
	if !found {
		return "ignored" // a room that was not filed from Slack
	}
	if _, err := a.Slack.Post(ctx, th.Channel, th.TS, line, nil); err != nil {
		a.log().Warn("slack post failed", "room", e.Room, "err", err)
		return "error"
	}
	return "posted"
}

// line is the event's thread template. Every value is clipped to the broker's own vocabulary
// first (M4), so no free text reaches Slack (invariant 7).
func (a *App) line(e webhook.Event) (string, bool) {
	url := strings.TrimSuffix(a.RoomsURL, "/") + "/r/" + e.Room
	reason, limit, state := webhook.Clip("reason", e.Reason), webhook.Clip("limit", e.Limit), webhook.Clip("state", e.State)
	switch e.Type {
	case "room.task_triaged":
		return "Triaged. Room: " + url, true
	case "room.run_started":
		return "A run started.", true
	case "room.waiting_for_human":
		return fmt.Sprintf("The work waits for a human (%s): answer it on the room page, %s. Nothing posted here reaches the run.", reason, url), true
	case "room.pr_opened":
		return fmt.Sprintf("Pull request #%d opened.", e.PR), true
	case "room.verdict_posted":
		return fmt.Sprintf("A review verdict is on #%d.", e.PR), true
	case "room.merged":
		return fmt.Sprintf("#%d merged.", e.PR), true
	case "room.reverted":
		return fmt.Sprintf("#%d reverted.", e.PR), true
	case "room.budget_reached":
		return fmt.Sprintf("A budget was reached (%s).", limit), true
	case "room.ended":
		return fmt.Sprintf("Ended, state: %s. Room: %s", state, url), true
	}
	return "", false
}
