// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Smana/maku/internal/envelope"
	"github.com/Smana/maku/internal/wire"
)

// QuickstartRoles are the roles roomctl run starts: a reviewer needs a pull request, and a new
// room names none.
var QuickstartRoles = []string{"implementer", "tester", "triager"}

// terminalPhases are runwatch.Terminal's (TestTerminalPhasesMatchRunwatch): a run_phase with one
// of them is the run's end.
var terminalPhases = map[string]bool{"Succeeded": true, "Failed": true, "BudgetExhausted": true, "Revoked": true}

// QuickstartRun is `roomctl run` (install-anywhere I7): on a local broker, one command makes the
// room, queues the task for the run's brief, asks for the run, applies the claim the broker
// rendered with the developer's own kubectl, and follows the room until the run ends. The broker
// never creates an AgentRun: the claim is runrequest's manifest, on the broker's runs seam, so the
// CLI cannot disagree with the chart about the kind, the namespace or the name prefix.
type QuickstartRun struct {
	Client     Client
	Repository string // owner/name
	Task       string
	Role       string // one of QuickstartRoles
	// Apply creates the claim from its JSON manifest: kubectl apply -f - under the developer's kubeconfig.
	Apply      func(ctx context.Context, manifest []byte) error
	Out        io.Writer
	ActTimeout time.Duration // per act; zero is a minute
	// SettleStep and SettleFor pace the wait for a new room's log row: the first act is retried,
	// the delay doubling from SettleStep (zero is 250ms, capped at 2s), until SettleFor (zero is 30s).
	SettleStep, SettleFor time.Duration
}

// RunEnd is where the run ended: its room, its claim (namespace/name), and the log's phase and reason.
type RunEnd struct{ Room, Claim, Phase, Reason string }

// StepError is QuickstartRun stopping at Step (room, task, start_run, claim, follow or run),
// with the cause as the broker or kubectl gave it.
type StepError struct {
	Step string
	Err  error
}

// Error names the step and its cause.
func (e *StepError) Error() string { return e.Step + ": " + e.Err.Error() }

// Unwrap returns the cause.
func (e *StepError) Unwrap() error { return e.Err }

// Do runs the steps in order and stops at the first that fails. It returns nil only when the run
// ended Succeeded.
func (q QuickstartRun) Do(ctx context.Context) (RunEnd, error) {
	if !slices.Contains(QuickstartRoles, q.Role) {
		return RunEnd{}, &StepError{"start_run", fmt.Errorf("role %q: roomctl run starts an implementer, a tester or a triager", q.Role)}
	}
	act := func(room string, action map[string]any) (wire.ServerFrame, error) {
		actx, cancel := context.WithTimeout(ctx, cmp.Or(q.ActTimeout, time.Minute))
		defer cancel()
		f, err := q.Client.Act(actx, room, action)
		if err == nil && f.Rejected != "" {
			err = fmt.Errorf("rejected: %s", Explain(f.Rejected))
		}
		return f, err
	}
	room, err := q.Client.CreateRoom(ctx, q.Repository, "public")
	if err != nil {
		return RunEnd{}, &StepError{"room", err}
	}
	end := RunEnd{Room: room}
	_, _ = fmt.Fprintf(q.Out, "room %s: %s/r/%s\n", room, q.Client.URL, room)
	// start_run carries no text: a run's task is its brief, which quotes what the room queued.
	f, err := q.settle(ctx, func() (wire.ServerFrame, error) {
		return act(room, map[string]any{"kind": "message", "delivery": "queued", "text": q.Task})
	})
	if err != nil {
		return end, &StepError{"task", err}
	}
	_, _ = fmt.Fprintf(q.Out, "task queued: seq %d\n", f.Seq)
	if f, err = act(room, map[string]any{"kind": "start_run", "role": q.Role}); err != nil {
		return end, &StepError{"start_run", err}
	}
	var claim struct {
		Metadata struct{ Name, Namespace string } `json:"metadata"`
	}
	if json.Unmarshal(f.Result, &claim) != nil || claim.Metadata.Name == "" {
		return end, &StepError{"start_run", errors.New("the broker sent no claim to apply: roomctl run needs a broker without a factory (the quickstart's)")}
	}
	end.Claim = OneLine(claim.Metadata.Namespace + "/" + claim.Metadata.Name)
	_, _ = fmt.Fprintf(q.Out, "run requested: %s\n", end.Claim)
	if err := q.Apply(ctx, f.Result); err != nil {
		return end, &StepError{"claim", err}
	}
	stop := func(ev envelope.Event) bool {
		if ev.Type != envelope.StateChanged || ev.RunID == "" || !ownRun(claim.Metadata.Name, ev.RunID) {
			return false
		}
		var p struct{ Kind, Phase, Reason string }
		if json.Unmarshal(ev.Payload, &p) != nil || p.Kind != "run_phase" || !terminalPhases[p.Phase] {
			return false
		}
		end.Phase, end.Reason = p.Phase, OneLine(p.Reason)
		return true
	}
	if err := q.Client.Follow(ctx, room, 50, q.Out, stop); err != nil {
		return end, &StepError{"follow", err}
	}
	if end.Phase == "" {
		return end, &StepError{"follow", fmt.Errorf("stopped before the run ended; it goes on: kubectl -n %s get agentrun %s",
			OneLine(claim.Metadata.Namespace), OneLine(claim.Metadata.Name))}
	}
	why := ""
	if end.Reason != "" {
		why = ": " + end.Reason
	}
	_, _ = fmt.Fprintf(q.Out, "run %s ended %s%s\n", end.Claim, end.Phase, why)
	if end.Phase != "Succeeded" {
		return end, &StepError{"run", fmt.Errorf("%s ended %s%s", end.Claim, end.Phase, why)}
	}
	return end, nil
}

// ownRun is whether run id is this claim's: the claim is named <prefix><id>, and every prefix ends
// in a dash, so a bare suffix match would take the tail of another run's id for this one.
func ownRun(claim, id string) bool { return claim == id || strings.HasSuffix(claim, "-"+id) }

// settle retries first while the room 404s. POST /api/rooms answers once the Room exists; its log
// row and seq 1 come from the reconciler a moment later, and the broker's WebSocket gate refuses
// a room without one. Nothing has been written until that act is accepted, so retrying is safe.
func (q QuickstartRun) settle(ctx context.Context, first func() (wire.ServerFrame, error)) (wire.ServerFrame, error) {
	step, limit := cmp.Or(q.SettleStep, 250*time.Millisecond), cmp.Or(q.SettleFor, 30*time.Second)
	deadline := time.Now().Add(limit)
	for {
		f, err := first()
		var ref *Refusal
		if !errors.As(err, &ref) || ref.Code != http.StatusNotFound || !time.Now().Add(step).Before(deadline) {
			return f, err
		}
		t := time.NewTimer(step)
		select {
		case <-ctx.Done():
			t.Stop()
			return f, err
		case <-t.C:
		}
		step = min(step*2, 2*time.Second)
	}
}
