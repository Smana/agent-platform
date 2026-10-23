// SPDX-License-Identifier: Apache-2.0

package runrequest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"github.com/Smana/agent-platform/internal/httpx"
)

// TaskRequest is one api task to create: the factory's POST /v1/tasks fields, the caller's own
// access token (C4) and the key that names the act. Neither of the last two is in the body.
type TaskRequest struct {
	Repository, Title, Text, DataClass, OnBehalfOf, Ref string
	MaxCostUSD                                          float64
	AccessToken, IdempotencyKey                         string
}

// TaskResult is the factory's answer: the task and its room (the same id), the issue filed (0
// when none), and whether the key had already created it.
type TaskResult struct {
	Task, Room string
	Issue      int
	Duplicate  bool
}

// ErrRefused is the factory refusing a task request: its status and its code, "refused" for a
// code outside POST /v1/tasks's closed list. A value, so errors.As matches it as written.
type ErrRefused struct {
	Status int
	Reason string
}

func (e ErrRefused) Error() string {
	return fmt.Sprintf("runrequest: factory refused (%d): %s", e.Status, e.Reason)
}

// refusalCodes is POST /v1/tasks's closed list of refusals the caller may read; anything else is
// clipped to "refused", so a factory's free text never reaches a model.
var refusalCodes = []string{"kill_switch", "bad_json", "idempotency_key_required", "bad_data_class", "bad_ref",
	"repository_not_allowed", "repository_not_owned", "meter_unpriced", "max_cost_required", "max_cost_over_principal",
	"max_cost_over_class", "not_linked", "not_a_maintainer", "sponsored_only", "invalid_draft", "bad_text", "tasks_unavailable",
	"principal_task_cap", "ledger_unavailable", "principal_budget_exhausted", "intake_failed", "idempotency_conflict"}

// CreateTask posts r to the factory's POST /v1/tasks with the caller's own token, as Request does.
func (f Factory) CreateTask(ctx context.Context, r TaskRequest) (TaskResult, error) {
	if r.AccessToken == "" { // never an unauthenticated or asserted request (C4)
		return TaskResult{}, ErrForbidden
	}
	if r.IdempotencyKey == "" {
		return TaskResult{}, ErrNoKey
	}
	body, err := json.Marshal(struct {
		Repository string  `json:"repository"`
		Title      string  `json:"title,omitempty"`
		Text       string  `json:"text"`
		DataClass  string  `json:"dataClass"`
		MaxCostUSD float64 `json:"maxCostUSD"`
		OnBehalfOf string  `json:"onBehalfOf,omitempty"`
		Ref        string  `json:"ref,omitempty"`
	}{r.Repository, r.Title, r.Text, r.DataClass, r.MaxCostUSD, r.OnBehalfOf, r.Ref})
	if err != nil {
		return TaskResult{}, fmt.Errorf("runrequest: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.URL+"/v1/tasks", bytes.NewReader(body))
	if err != nil {
		return TaskResult{}, fmt.Errorf("runrequest: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", r.IdempotencyKey)
	hc := f.HC
	if hc == nil {
		hc = httpx.New(factoryTimeout, nil)
	}
	resp, err := hc.Do(req)
	if err != nil {
		// The client's error names the URL, never the headers: the token stays out of it.
		return TaskResult{}, fmt.Errorf("runrequest: factory: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := httpx.ReadBody(resp.Body, maxResponse)
	if err != nil {
		return TaskResult{}, fmt.Errorf("runrequest: factory: %w", err)
	}
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
		var out struct {
			Task      string `json:"task"`
			Room      string `json:"room"`
			Issue     int    `json:"issue"`
			Duplicate bool   `json:"duplicate"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return TaskResult{}, fmt.Errorf("runrequest: factory answer: %w", err)
		}
		return TaskResult{Task: out.Task, Room: out.Room, Issue: out.Issue, Duplicate: out.Duplicate}, nil
	}
	if resp.StatusCode >= 400 {
		var out struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &out)
		reason := "refused"
		if slices.Contains(refusalCodes, out.Error) {
			reason = out.Error
		}
		return TaskResult{}, ErrRefused{Status: resp.StatusCode, Reason: reason}
	}
	return TaskResult{}, fmt.Errorf("runrequest: factory: %s", resp.Status)
}
