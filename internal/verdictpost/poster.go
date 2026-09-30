// SPDX-License-Identifier: Apache-2.0

// Package verdictpost carries an agent's review verdict to its pull request (SP2
// design §3, rulings P28–P30): one comment per verdict, posted by the leader, with
// the outcome recorded in the room.
package verdictpost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/github"
	"github.com/Smana/agent-platform/internal/store"
)

const (
	// Window bounds the sweep: a verdict older than this when the App first
	// works stays in the room only.
	Window = 24 * time.Hour
	// batch is how many verdicts one tick takes, and postTimeout bounds each
	// one's search and post.
	batch       = 20
	postTimeout = time.Minute
)

// Log is what the poster reads and records; the broker's store implements it.
type Log interface {
	UnpostedVerdicts(ctx context.Context, since time.Time, limit int) ([]envelope.Event, error)
	Append(ctx context.Context, d envelope.Draft) (envelope.Event, bool, error)
}

// Commenter posts one comment per marker; *github.App implements it.
type Commenter interface {
	Enabled() bool
	Comment(ctx context.Context, pr, marker, body string) (string, error)
}

// Poster posts agents' verdicts. Log, GitHub, DataClass, Now and Leading are
// required. Leading reports that this replica holds the leader lease (ruling
// SY): it is checked before every post, and nil posts nothing. OnResult counts
// posted, not_posted and error.
type Poster struct {
	Log       Log
	GitHub    Commenter
	DataClass func(ctx context.Context, room string) string
	PublicURL string
	Now       func() time.Time
	Leading   func() bool
	OnResult  func(ctx context.Context, result string)
	Logger    *slog.Logger

	notBefore time.Time // GitHub's rate limit lifts then
}

// Marker ends the verdict's comment, and finds it again (ruling P30).
func Marker(room string, seq int64) string {
	return fmt.Sprintf("<!-- agent-room:%s:%d -->", room, seq)
}

var (
	titles   = map[string]string{"approve": "approved", "changes": "changes requested"}
	outcomes = map[string]string{"verdict_posted": "posted", "verdict_not_posted": "not_posted"}
)

// Body is the comment without its marker. Only a public room's summary leaves
// the room (C7), neutralised: it is an agent's text, so it notifies nobody,
// loads nothing and renders no HTML, a marker included (review M5, m3).
func Body(ev envelope.Event, v envelope.MessagePayload, dataClass, publicURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### Agent review: %s\n\n", titles[v.Verdict])
	if dataClass == "public" {
		for _, line := range strings.Split(github.Neutralise(v.Text), "\n") {
			b.WriteString("> " + line + "\n")
		}
	} else {
		b.WriteString("_The summary stays in the room: its data class is not public._\n")
	}
	fmt.Fprintf(&b, "\nRecorded by the %s run `%s` at `%s`, in room [%s](%s/r/%s), event %d. "+
		"An agent verdict is advice: it neither approves nor blocks this pull request.",
		ev.Actor.Role, ev.RunID, v.Commit, ev.RoomID, publicURL, ev.RoomID, ev.Seq)
	return b.String()
}

// Once posts the unposted verdicts, while this replica leads. A transient
// failure leaves a verdict for the next tick, where the App's own marked
// comment makes a repeat a no-op; a rate limit holds the poster until it lifts.
func (p *Poster) Once(ctx context.Context) error {
	if !p.GitHub.Enabled() || p.Now().Before(p.notBefore) {
		return nil // no App key yet (ruling P31): verdicts wait, up to Window
	}
	evs, err := p.Log.UnpostedVerdicts(ctx, p.Now().Add(-Window), batch)
	if err != nil {
		return fmt.Errorf("verdictpost: %w", err)
	}
	for _, ev := range evs {
		if ctx.Err() != nil || p.Leading == nil || !p.Leading() {
			return nil
		}
		var v envelope.MessagePayload
		if json.Unmarshal(ev.Payload, &v) != nil || v.PullRequest == "" {
			p.record(ctx, ev, "verdict_not_posted", map[string]any{"reason": "no_pull_request"})
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, postTimeout)
		url, err := p.GitHub.Comment(pctx, v.PullRequest, Marker(ev.RoomID, ev.Seq),
			Body(ev, v, p.DataClass(ctx, ev.RoomID), p.PublicURL))
		cancel()
		var ae *github.APIError
		switch {
		case err == nil:
			p.record(ctx, ev, "verdict_posted", map[string]any{"url": url})
		case errors.Is(err, github.ErrNotAPullRequest):
			p.record(ctx, ev, "verdict_not_posted", map[string]any{"reason": "no_pull_request"})
		case github.Permanent(err):
			p.record(ctx, ev, "verdict_not_posted", map[string]any{"reason": "github_refused", "detail": detail(err)})
		default:
			p.result(ctx, "error")
			p.log().Warn("verdict not posted yet", "room", ev.RoomID, "seq", ev.Seq, "err", err.Error())
			if errors.As(err, &ae) && ae.RateLimited {
				p.notBefore = p.Now().Add(ae.RetryAfter)
				return nil
			}
		}
	}
	return nil
}

// detail names a refusal by a fixed code: the record is broker-origin text in
// the room, so it carries no GitHub path or message.
func detail(err error) string {
	var ae *github.APIError
	if errors.As(err, &ae) {
		return fmt.Sprintf("http_%d", ae.Status)
	}
	return "too_many_comments"
}

func (p *Poster) record(ctx context.Context, verdict envelope.Event, kind string, fields map[string]any) {
	fields["verdictSeq"] = verdict.Seq
	cause := verdict.Seq
	if _, _, err := p.Log.Append(ctx, envelope.Draft{RoomID: verdict.RoomID, RunID: verdict.RunID,
		Actor: envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"}, Type: envelope.StateChanged,
		CausedBy: &cause, Origin: envelope.OriginBroker, OriginClient: store.VerdictsClient, OriginSeq: verdict.Seq,
		Payload: envelope.StatePayload(kind, fields)}); err != nil {
		p.result(ctx, "error")
		p.log().Error("record a verdict's outcome", "room", verdict.RoomID, "seq", verdict.Seq, "kind", kind, "err", err.Error())
		return
	}
	p.result(ctx, outcomes[kind])
}

func (p *Poster) result(ctx context.Context, r string) {
	if p.OnResult != nil {
		p.OnResult(ctx, r)
	}
}

func (p *Poster) log() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.New(slog.DiscardHandler)
}
