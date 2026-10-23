// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/mcpwire"
	"github.com/Smana/agent-platform/internal/runrequest"
	"github.com/Smana/agent-platform/internal/sponsored"
)

// TaskCreator is the factory's intake api, as the broker reaches it with the caller's own token.
type TaskCreator interface {
	CreateTask(ctx context.Context, r runrequest.TaskRequest) (runrequest.TaskResult, error)
}

// maxIdempotencyKey keeps the key the factory sees, system:<name>:<key>, under its 255.
const maxIdempotencyKey = 128

// runCreateSchema requires max_cost (F4: "refused without max_cost"); additionalProperties false
// keeps class, tier and auto-merge out (F8). idempotency_key is at most 128, so the key the
// factory sees, system:<name>:<key>, stays under its 255.
var runCreateSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["repo","text","dataClass","max_cost"],` +
	`"properties":{"repo":{"type":"string","pattern":"^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$"},"title":{"type":"string","maxLength":1024},` +
	`"text":{"type":"string","minLength":1,"maxLength":16384},"dataClass":{"enum":["public"]},"max_cost":{"type":"number","exclusiveMinimum":0},` +
	`"on_behalf_of":{"type":"string","pattern":"^slack:T[A-Z0-9]{2,20}/[UW][A-Z0-9]{2,20}$"},"ref":{"type":"string","maxLength":256},` +
	`"idempotency_key":{"type":"string","minLength":1,"maxLength":128}}}`)

var onBehalfRE = regexp.MustCompile(`^slack:T[A-Z0-9]{2,20}/[UW][A-Z0-9]{2,20}$`)

type runCreateArgs struct {
	Repo           string  `json:"repo"`
	Title          string  `json:"title"`
	Text           string  `json:"text"`
	DataClass      string  `json:"dataClass"`
	MaxCost        float64 `json:"max_cost"`
	OnBehalfOf     string  `json:"on_behalf_of"`
	Ref            string  `json:"ref"`
	IdempotencyKey string  `json:"idempotency_key"`
}

// createTask asks the factory for an api task with the caller's own token (C4): the factory
// authenticates it again and decides everything (R4). Only a sponsored principal reaches it. The
// text passes the broker's redactor whatever it is (C12: the factory has none), a titled draft
// issue_draft's whole check. The key defaults to the arguments' hash, so a retried call creates
// one task.
func (s *Server) createTask(ctx context.Context, p authn.Principal, args json.RawMessage) (runrequest.TaskResult, error) {
	if !p.Sponsored {
		return runrequest.TaskResult{}, toolError("unknown tool: run_create")
	}
	var a runCreateArgs
	if mcpwire.DecodeArgs(args, &a) != nil || !repositoryRE.MatchString(a.Repo) || a.MaxCost <= 0 || a.Text == "" ||
		a.DataClass != "public" || len(a.IdempotencyKey) > maxIdempotencyKey {
		return runrequest.TaskResult{}, toolError("invalid_arguments: repo, text, dataClass public and max_cost above 0 are required; " +
			"idempotency_key is at most 128 characters")
	}
	if !sponsored.ValidRef(a.Ref) {
		return runrequest.TaskResult{}, toolError("bad_ref: ref is a git ref of at most 256 characters")
	}
	if a.OnBehalfOf != "" && !onBehalfRE.MatchString(a.OnBehalfOf) {
		return runrequest.TaskResult{}, toolError("invalid_arguments: on_behalf_of is slack:<team>/<user>")
	}
	if a.Title != "" {
		d, err := s.checkDraft(ctx, a.Title, a.Text)
		if err != nil {
			return runrequest.TaskResult{}, err
		}
		a.Title, a.Text = d.Title, d.Body
	} else if err := s.redactCheck(ctx, a.Text); err != nil {
		return runrequest.TaskResult{}, err
	}
	if s.Tasks == nil {
		return runrequest.TaskResult{}, toolError("no_factory: the factory is not configured")
	}
	key := a.IdempotencyKey
	if key == "" {
		sum := sha256.Sum256(args)
		key = hex.EncodeToString(sum[:16])
	}
	res, err := s.Tasks.CreateTask(ctx, runrequest.TaskRequest{Repository: a.Repo, Title: a.Title, Text: a.Text,
		DataClass: a.DataClass, MaxCostUSD: a.MaxCost, OnBehalfOf: a.OnBehalfOf, Ref: a.Ref,
		AccessToken: p.AccessToken, IdempotencyKey: p.ID + ":" + key})
	var refused runrequest.ErrRefused
	if errors.As(err, &refused) {
		return runrequest.TaskResult{}, toolError("rejected: " + refused.Reason)
	}
	return res, err
}

// redactCheck refuses text holding what the redactor flags, failing closed without a redactor.
func (s *Server) redactCheck(ctx context.Context, text string) error {
	if s.Actor == nil || s.Actor.Redactor == nil {
		return errors.New("humanapi: no redactor")
	}
	_, rules, err := s.Actor.Redactor.Payload(ctx, envelope.Must(map[string]string{"text": text}))
	if err != nil {
		return err
	}
	if len(rules) > 0 {
		return toolError("invalid_draft: it holds what looks like a secret (" + strings.Join(rules, ", ") +
			"): remove it, since the factory's agents read the task in full")
	}
	return nil
}

// runCreated is run_create's answer without a task: the factory's, and the room page.
func (s *Server) runCreated(r runrequest.TaskResult) map[string]any {
	return map[string]any{"task": r.Task, "room": r.Room, "issue": r.Issue, "duplicate": r.Duplicate, "url": s.PublicURL + "/r/" + r.Room}
}
