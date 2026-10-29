// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	factoryv1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/authn"
	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/finding"
	"github.com/Smana/maku/internal/factory/fmetrics"
	"github.com/Smana/maku/internal/factory/sanitize"
	"github.com/Smana/maku/internal/factory/taskid"
)

// createInternalTask is POST /v1/tasks for an internal principal (RA2–RA6): the finding's gates,
// today's dedup with generations, an investigate task on internal data and a public stub issue.
// It grants nothing a sponsored principal has beyond its own keys: no title, no onBehalfOf, no
// ref, no data class but internal, only its own repositories and ceilings.
func (s *Server) createInternalTask(w http.ResponseWriter, r *http.Request, p authn.Principal, ip *config.InternalPrincipal) {
	ctx := fmetrics.WithPrincipal(r.Context(), p.ID)
	out := func(code int, reason string) { s.countIntake(ctx, reason); refuse(w, code, reason) }
	fail := func(reason string, err error) {
		if s.Log != nil {
			s.Log.ErrorContext(ctx, "intake: "+reason, "principal", p.ID, "error", err)
		}
		out(http.StatusServiceUnavailable, reason)
	}
	if s.Stopped(ctx) {
		out(http.StatusServiceUnavailable, "kill_switch")
		return
	}
	var in TaskRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		out(http.StatusBadRequest, "bad_json")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	inc := in.Incident
	switch {
	case key == "" || len(key) > maxKeyLen:
		out(http.StatusBadRequest, "idempotency_key_required")
	case in.Title != "":
		out(http.StatusBadRequest, "title_not_allowed")
	case in.OnBehalfOf != "":
		out(http.StatusBadRequest, "on_behalf_of_not_allowed")
	case in.Ref != "":
		out(http.StatusBadRequest, "bad_ref")
	case in.DataClass != "internal":
		out(http.StatusBadRequest, "bad_data_class")
	case inc == nil || inc.Alert == "" || inc.Resource == "":
		out(http.StatusBadRequest, "incident_required")
	case !slices.Contains(ip.Repositories, in.Repository):
		out(http.StatusForbidden, "repository_not_allowed")
	case s.Owned == nil || !s.Owned(in.Repository):
		out(http.StatusConflict, "repository_not_owned")
	case len(s.Cfg.Meter.Prices) == 0:
		out(http.StatusServiceUnavailable, "meter_unpriced")
	case !(in.MaxCostUSD >= minMaxCostUSD) || config.MicroUSD(in.MaxCostUSD) <= 0:
		out(http.StatusBadRequest, "max_cost_required")
	case in.MaxCostUSD > ip.MaxCostUSD:
		out(http.StatusBadRequest, "max_cost_over_principal")
	case config.MicroUSD(in.MaxCostUSD) > s.Cfg.MaxTaskMicroUSD():
		out(http.StatusBadRequest, "max_cost_over_class")
	case (inc.Verdict != "action_required" && inc.Verdict != "action_suggested") || inc.Confidence < ip.MinConfidence:
		s.countIntake(ctx, "below_threshold")
		reply(w, http.StatusAccepted, map[string]any{"accepted": false, "reason": "below_threshold"})
	default:
		s.admitInternal(ctx, w, out, fail, ip, key, in)
	}
}

// admitInternal runs the gates that read the cluster, in R5's order, then creates the Task before
// its issue: the name carries the generation, so a racing replay loses on AlreadyExists.
func (s *Server) admitInternal(ctx context.Context, w http.ResponseWriter, out func(int, string),
	fail func(string, error), ip *config.InternalPrincipal, key string, in TaskRequest) {
	inc := in.Incident
	base := "api:" + ip.Name + ":" + key
	group := taskid.Name(base)
	var same factoryv1.TaskList
	if err := s.Rooms.List(ctx, &same, client.InNamespace(s.Namespace), client.MatchingLabels{finding.LabelSourceKey: group}); err != nil {
		fail("tasks_unavailable", err)
		return
	}
	for i := range same.Items {
		if t := &same.Items[i]; !factoryv1.TerminalPhase(t.Status.Phase) {
			s.countIntake(ctx, "duplicate")
			reply(w, http.StatusOK, map[string]any{"task": t.Name, "room": t.Name, "issue": t.Spec.Issue, "duplicate": true})
			return
		}
	}
	switch capped, err := s.dailyCapReached(ctx); {
	case err != nil:
		fail("tasks_unavailable", err)
		return
	case capped:
		out(http.StatusTooManyRequests, "daily_task_cap")
		return
	}
	today, err := s.tasksToday(ctx, ip.Principal())
	switch {
	case err != nil:
		fail("tasks_unavailable", err)
		return
	case today >= ip.DailyTasks:
		out(http.StatusTooManyRequests, "principal_daily_tasks")
		return
	}
	switch n, err := s.openTasks(ctx, ip.Principal()); {
	case err != nil:
		fail("tasks_unavailable", err)
		return
	case n >= ip.OpenTaskCap():
		out(http.StatusTooManyRequests, "principal_task_cap")
		return
	}
	maxUSD := config.MicroUSD(in.MaxCostUSD)
	switch over, err := s.principalSpent(ctx, &ip.Sponsored, maxUSD); {
	case err != nil:
		fail("ledger_unavailable", err)
		return
	case over:
		out(http.StatusTooManyRequests, reasonPrincipalOver)
		return
	}

	name := taskid.Name(fmt.Sprintf("%s:gen%d", base, len(same.Items)+1))
	text := finding.Text("RunLore: "+inc.Alert+" on "+inc.Resource, inc.Verdict, inc.Confidence,
		inc.Resource, inc.Alert, inc.Severity, inc.Cluster, in.Text)
	sum := sha256.Sum256([]byte(text))
	text, _ = sanitize.Text(text)
	if m := s.Cfg.Caps.MaxTextBytes; m > 0 && len(text) > m {
		text = strings.ToValidUTF8(text[:max(m-64, 0)], "") + "\n[finding truncated by the factory]"
	}
	t := &factoryv1.Task{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace,
		Labels: map[string]string{finding.LabelSourceKey: group}},
		Spec: factoryv1.TaskSpec{Source: factoryv1.Source{Kind: "api", Ref: "api:" + ip.Name, Key: base,
			RequestedBy: ip.Principal(), Sponsor: factoryv1.Person(ip.Sponsor), Trust: "untrusted",
			ContentSHA256: hex.EncodeToString(sum[:])},
			Repository: in.Repository, Text: text, DataClass: "internal", MaxMicroUSD: maxUSD}}
	if err := s.Rooms.Create(ctx, t); apierrors.IsAlreadyExists(err) {
		s.countIntake(ctx, "duplicate") // the other replica won
		reply(w, http.StatusOK, map[string]any{"task": name, "room": name, "duplicate": true})
		return
	} else if err != nil {
		fail("intake_failed", err)
		return
	}
	filer := s.Forges
	if filer == nil || filer(in.Repository) == nil {
		fail("intake_failed", errNoForge) // the Task exists; a replay answers it, and an operator sees it without an issue
		return
	}
	title, body := finding.StubIssue(inc.Alert, inc.Resource, inc.Severity, inc.Verdict, inc.Confidence, name, s.Cfg.RoomsURL)
	// Past the Task, a client that hangs up must not orphan the issue: filing and linking outlive ctx.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	iss, err := filer(in.Repository).CreateIssue(fctx, title, body, []string{"factory/proposed"})
	if err != nil {
		fail("intake_failed", err)
		return
	}
	if err := s.linkIssue(fctx, name, iss.Number, true); err != nil {
		fail("intake_failed", err)
		return
	}
	s.countIntake(ctx, "admitted")
	if s.Metrics != nil {
		s.Metrics.MaxCost(ctx, maxUSD)
	}
	reply(w, http.StatusCreated, map[string]any{"task": name, "room": name, "issue": iss.Number})
}

// tasksToday counts the principal's tasks created this UTC day, whatever their kind, so the
// retiring /intake/runlore's tasks count against dailyTasks while both intakes run (R5).
func (s *Server) tasksToday(ctx context.Context, principal string) (int, error) {
	var l factoryv1.TaskList
	if err := s.Rooms.List(ctx, &l, client.InNamespace(s.Namespace)); err != nil {
		return 0, err
	}
	day, n := s.Now().UTC().Format(time.DateOnly), 0
	for i := range l.Items {
		if t := &l.Items[i]; t.Spec.Source.RequestedBy == principal && t.CreationTimestamp.UTC().Format(time.DateOnly) == day {
			n++
		}
	}
	return n, nil
}

// dailyCapReached reports whether the factory's daily task cap is spent, counted as the reconciler's
// admit counts it. A request it would reject is refused before its Task and its issue exist: the
// owner then never reads a stub that says the task investigates (gcp-0, task 2d5ch27g). Two racing
// requests can still pass it; the reconciler rejects the second and narrates that on its issue.
// An unset cap is no refusal here: the config refuses one, and the reconciler stays the authority.
func (s *Server) dailyCapReached(ctx context.Context) (bool, error) {
	limit := s.Cfg.Caps.TasksPerDay
	if limit < 1 {
		return false, nil
	}
	var l factoryv1.TaskList
	if err := s.Rooms.List(ctx, &l, client.InNamespace(s.Namespace)); err != nil {
		return false, err
	}
	n, at := 0, s.Now()
	for i := range l.Items {
		if factoryv1.SpendsDailyCap(&l.Items[i], at) {
			n++
		}
	}
	return n >= limit, nil
}
