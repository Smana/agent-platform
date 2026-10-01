// SPDX-License-Identifier: Apache-2.0

// Package runrequest asks for the next run. Only the factory creates AgentRuns
// (C3): with SP3, the broker calls POST /v1/runs with the human's own access token
// (C4); before SP3, it renders the claim for the owner to create (ruling P14).
package runrequest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Smana/agent-platform/internal/httpx"
	"github.com/Smana/agent-platform/internal/runwatch"
)

var (
	// ErrBudget is the factory refusing a run over the principal's budget (429).
	ErrBudget = errors.New("over_budget")
	// ErrForbidden is the factory refusing the principal (403), or no token to present.
	ErrForbidden = errors.New("forbidden")
	// ErrNoKey is a factory request with no idempotency key, refused before the call.
	ErrNoKey = errors.New("runrequest: a factory request needs an idempotency key")
)

// factoryTimeout bounds one POST /v1/runs; maxResponse its answer.
const (
	factoryTimeout = 15 * time.Second
	maxResponse    = 64 << 10
)

// Request is one run to start: the claim's fields, the principal asking, and
// that principal's own access token for the factory (C4). IdempotencyKey names
// the act that asks (review 4.4 I1): the same key always means the same run.
type Request struct {
	Role, Repository, BaseRef, Branch, TaskText, TaskURL, DataClass, RoomRef, Principal, AccessToken string
	IdempotencyKey                                                                                   string
	EgressProfiles                                                                                   []string
}

// Result is the requested run: its id, how it was requested ("manifest" or
// "factory"), and before SP3 the rendered claim.
type Result struct {
	RunID    string          `json:"runId"`
	Via      string          `json:"via"`
	Manifest json.RawMessage `json:"manifest,omitempty"`
}

// Requester asks for a run.
type Requester interface {
	Request(ctx context.Context, r Request) (Result, error)
}

// NewID is a fresh C2 id: 8 characters of [a-z2-7], 40 random bits.
func NewID() string { return strings.ToLower(rand.Text()[:8]) }

// idFor is the run id of an idempotency key: a retried act renders the same
// claim name, so applying both claims creates one run. No key, a fresh id.
func idFor(key string) string {
	if key == "" {
		return NewID()
	}
	sum := sha256.Sum256([]byte(key))
	return strings.ToLower(base32.StdEncoding.EncodeToString(sum[:5]))
}

func task(r Request) map[string]string {
	if r.TaskURL != "" {
		return map[string]string{"url": r.TaskURL}
	}
	return map[string]string{"text": r.TaskText}
}

// Manifest renders the AgentRun claim for the owner to create (ruling P14), on
// runwatch's seam: its GVK and namespace, and the xplane-run- prefix it parses.
type Manifest struct{}

// Request renders r as a claim, its run id derived from r's idempotency key.
func (Manifest) Request(_ context.Context, r Request) (Result, error) {
	id := idFor(r.IdempotencyKey)
	spec := map[string]any{"role": r.Role, "repository": r.Repository, "baseRef": r.BaseRef, "branch": r.Branch,
		"principal": r.Principal, "dataClass": r.DataClass, "roomRef": r.RoomRef, "task": task(r)}
	if len(r.EgressProfiles) > 0 {
		spec["egress"] = map[string]any{"profiles": r.EgressProfiles}
	}
	gvk := runwatch.GVK()
	b, err := json.Marshal(map[string]any{"apiVersion": gvk.GroupVersion().String(), "kind": gvk.Kind,
		"metadata": map[string]string{"name": "xplane-run-" + id, "namespace": runwatch.Namespace}, "spec": spec})
	if err != nil {
		return Result{}, fmt.Errorf("runrequest: render: %w", err)
	}
	return Result{RunID: id, Via: "manifest", Manifest: b}, nil
}

// Factory is SP3's run API at URL. HC nil means the audited egress client.
type Factory struct {
	URL string
	HC  *http.Client
}

// Request posts r to the factory as the human it names, with their own token.
func (f Factory) Request(ctx context.Context, r Request) (Result, error) {
	if r.AccessToken == "" { // never an unauthenticated or asserted request (C4)
		return Result{}, ErrForbidden
	}
	if r.IdempotencyKey == "" { // a retry without one would create a second run
		return Result{}, ErrNoKey
	}
	body, err := json.Marshal(map[string]any{"role": r.Role, "repository": r.Repository, "baseRef": r.BaseRef,
		"task": task(r), "dataClass": r.DataClass, "roomRef": r.RoomRef, "egressProfiles": r.EgressProfiles})
	if err != nil {
		return Result{}, fmt.Errorf("runrequest: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.URL+"/v1/runs", bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("runrequest: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	// SP3's contract: the same key answers the same runId, so a retried act
	// never creates a second run (docs/integration.md).
	req.Header.Set("Idempotency-Key", r.IdempotencyKey)
	hc := f.HC
	if hc == nil {
		hc = httpx.New(factoryTimeout, nil)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("runrequest: factory: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusCreated:
		raw, err := httpx.ReadBody(resp.Body, maxResponse)
		if err != nil {
			return Result{}, fmt.Errorf("runrequest: factory: %w", err)
		}
		var out Result
		if err := json.Unmarshal(raw, &out); err != nil {
			return Result{}, fmt.Errorf("runrequest: factory answer: %w", err)
		}
		return Result{RunID: out.RunID, Via: "factory"}, nil
	case http.StatusTooManyRequests:
		return Result{}, ErrBudget
	case http.StatusForbidden:
		return Result{}, ErrForbidden
	}
	return Result{}, fmt.Errorf("runrequest: factory: %s", resp.Status)
}
