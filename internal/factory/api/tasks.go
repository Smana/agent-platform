// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	factoryv1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/fmetrics"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/sanitize"
	"github.com/Smana/agent-platform/internal/factory/taskid"
)

// IssueFiler files an issue as the factory App: RunLore's CreateIssue, whose answer is a
// forge.Issue since the splitter (#195).
type IssueFiler interface {
	CreateIssue(ctx context.Context, title, body string, labels []string) (forge.Issue, error)
}

// TaskRequest is POST /v1/tasks's body (F4). Unknown fields are refused: class, tier, template,
// auto-merge and egress are never the caller's (F8, R11).
type TaskRequest struct {
	Repository string  `json:"repository"`
	Title      string  `json:"title,omitempty"`
	Text       string  `json:"text"`
	DataClass  string  `json:"dataClass"`
	MaxCostUSD float64 `json:"maxCostUSD"`
	OnBehalfOf string  `json:"onBehalfOf,omitempty"`
	Ref        string  `json:"ref,omitempty"`
}

const (
	reasonSponsoredOnly = "sponsored_only"
	reasonPrincipalOver = "principal_budget_exhausted"
	maxRefLen           = 256
	minMaxCostUSD       = 0.01 // the smallest ceiling an api task may ask for (I-2)
)

// errReplayConflict is a Task of the key's name that is not this principal's for this key.
var errReplayConflict = errors.New("api: the name belongs to another request")

// errNoForge is a titled request on a repository with no GitHub connection (C24).
var errNoForge = errors.New("api: no GitHub connection for the repository")

// createTask is the intake api (F4, R4): a sponsored principal's request becomes a Task, which
// triage, the gate and narration then treat as any other. Refusals name codes only.
func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	p, err := s.Auth.Authenticate(r)
	switch {
	case errors.Is(err, authn.ErrForbidden):
		refuse(w, http.StatusForbidden, "not_permitted")
		return
	case err != nil:
		refuse(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	sp := s.Cfg.SponsoredBy(p.ID)
	if !p.Sponsored || sp == nil {
		refuse(w, http.StatusForbidden, reasonSponsoredOnly)
		return
	}
	ctx := fmetrics.WithPrincipal(r.Context(), p.ID)
	out := func(code int, reason string) {
		s.countIntake(ctx, reason)
		refuse(w, code, reason)
	}
	fail := func(code int, reason string, err error) { // the cause is logged, never answered
		if s.Log != nil {
			s.Log.ErrorContext(ctx, "intake: "+reason, "principal", p.ID, "error", err)
		}
		out(code, reason)
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
	switch {
	case key == "" || len(key) > maxKeyLen:
		out(http.StatusBadRequest, "idempotency_key_required")
		return
	case in.DataClass != "public": // R37: internal data is the admins', and a machine is none
		out(http.StatusBadRequest, "bad_data_class")
		return
	case len(in.Ref) > maxRefLen:
		out(http.StatusBadRequest, "bad_ref")
		return
	case !slices.Contains(sp.Repositories, in.Repository): // exact: the config is spelled as listed (C22)
		out(http.StatusForbidden, "repository_not_allowed")
		return
	case s.Owned == nil || !s.Owned(in.Repository):
		out(http.StatusConflict, "repository_not_owned")
		return
	}
	// The float is range-checked before it becomes micro-USD: 1e300 would overflow the conversion.
	switch {
	case len(s.Cfg.Meter.Prices) == 0: // C13: a ceiling in dollars needs priced runs; before the ceiling checks
		out(http.StatusServiceUnavailable, "meter_unpriced")
		return
	case !(in.MaxCostUSD >= minMaxCostUSD): // under a cent a third of it rounds a cap to 0, which reads as unset
		out(http.StatusBadRequest, "max_cost_required")
		return
	case in.MaxCostUSD > sp.MaxCostUSD:
		out(http.StatusBadRequest, "max_cost_over_principal")
		return
	}
	maxUSD := config.MicroUSD(in.MaxCostUSD)
	switch {
	case maxUSD <= 0:
		out(http.StatusBadRequest, "max_cost_required")
		return
	case maxUSD > s.Cfg.MaxTaskMicroUSD():
		out(http.StatusBadRequest, "max_cost_over_class")
		return
	}
	login, reason := s.sponsorOf(sp, in)
	if reason != "" {
		out(http.StatusForbidden, reason)
		return
	}
	title, text, err := cleanRequest(in)
	if err != nil {
		out(http.StatusBadRequest, err.Error())
		return
	}
	// What reaches the issue and the agents is scanned before any side effect, failing closed.
	if s.Redact == nil {
		fail(http.StatusServiceUnavailable, "intake_failed", errors.New("api: no redactor"))
		return
	}
	if _, rules := s.Redact.String(title + "\n" + text); len(rules) > 0 {
		out(http.StatusBadRequest, "invalid_draft")
		return
	}
	name := taskid.Name("api:" + sp.Name + ":" + key)
	if t, found, err := s.existing(ctx, name); err != nil {
		fail(http.StatusServiceUnavailable, "tasks_unavailable", err)
		return
	} else if found {
		if !replayOf(t, sp, key) {
			out(http.StatusConflict, "idempotency_conflict")
			return
		}
		s.countIntake(ctx, "duplicate")
		reply(w, http.StatusOK, map[string]any{"task": t.Name, "room": t.Name, "issue": t.Spec.Issue, "duplicate": true})
		return
	}
	switch n, err := s.openTasks(ctx, sp.Principal()); {
	case err != nil:
		fail(http.StatusServiceUnavailable, "tasks_unavailable", err)
		return
	case n >= sp.OpenTaskCap(): // C21
		out(http.StatusTooManyRequests, "principal_task_cap")
		return
	}
	switch over, err := s.principalSpent(ctx, sp, maxUSD); {
	case err != nil:
		fail(http.StatusServiceUnavailable, "ledger_unavailable", err)
		return
	case over:
		out(http.StatusTooManyRequests, reasonPrincipalOver)
		return
	}
	code, body, err := s.admitTask(ctx, sp, login, name, key, title, text, in, maxUSD)
	switch {
	case errors.Is(err, errReplayConflict):
		out(http.StatusConflict, "idempotency_conflict")
		return
	case err != nil:
		fail(http.StatusServiceUnavailable, "intake_failed", err)
		return
	}
	if code == http.StatusCreated {
		s.countIntake(ctx, "admitted")
		if s.Metrics != nil {
			s.Metrics.MaxCost(ctx, maxUSD)
		}
	} else {
		s.countIntake(ctx, "duplicate") // the AlreadyExists race: another request admitted it first
	}
	reply(w, code, body)
}

// countIntake records one request's outcome; a Server without metrics records nothing.
func (s *Server) countIntake(ctx context.Context, outcome string) {
	if s.Metrics != nil {
		s.Metrics.APIIntake(ctx, outcome)
	}
}

// sponsorOf is the request's sponsor login (R1, R3, R9): a linked user's maintainer, else the
// config's sponsor, re-checked as a maintainer of the repository either way.
func (s *Server) sponsorOf(sp *config.Sponsored, in TaskRequest) (string, string) {
	login := sp.Sponsor
	if in.OnBehalfOf != "" {
		id, found := strings.CutPrefix(in.OnBehalfOf, "slack:")
		if !found {
			return "", "not_linked" // only slack:<team>/<user> links exist
		}
		var ok bool
		if login, ok = sp.OnBehalfOf.Slack[id]; !ok {
			return "", "not_linked"
		}
	}
	if !s.Cfg.RepositoryFor(in.Repository).IsMaintainer(login) {
		return "", "not_a_maintainer"
	}
	return strings.ToLower(login), ""
}

// cleanRequest strips hidden characters (F8) and, with a title, holds the draft to the template.
func cleanRequest(in TaskRequest) (string, string, error) {
	if in.Title == "" {
		text, _ := sanitize.Hidden(strings.TrimSpace(in.Text))
		if text == "" || len(text) > 16<<10 {
			return "", "", errors.New("bad_text")
		}
		return "", text, nil
	}
	d, err := sanitize.CheckDraft(in.Title, in.Text)
	if err != nil {
		return "", "", errors.New("invalid_draft")
	}
	return d.Title, d.Body, nil
}

// replayOf is whether t is this principal's task for this key: a name collision under another key
// or principal is a conflict, never a replay.
func replayOf(t *factoryv1.Task, sp *config.Sponsored, key string) bool {
	return t.Spec.Source.Key == "api:"+sp.Name+":"+key && t.Spec.Source.RequestedBy == sp.Principal()
}

func (s *Server) existing(ctx context.Context, name string) (*factoryv1.Task, bool, error) {
	var t factoryv1.Task
	err := s.Rooms.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: name}, &t)
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	return &t, err == nil, err
}

// openTasks counts the principal's api tasks not yet ended (C21): each holds an issue and a slot
// of caps.tasksPerDay, which humans share.
func (s *Server) openTasks(ctx context.Context, principal string) (int, error) {
	var l factoryv1.TaskList
	if err := s.Rooms.List(ctx, &l, client.InNamespace(s.Namespace)); err != nil {
		return 0, err
	}
	n := 0
	for i := range l.Items {
		t := &l.Items[i]
		if t.Spec.Source.Kind == "api" && t.Spec.Source.RequestedBy == principal && !factoryv1.TerminalPhase(t.Status.Phase) {
			n++
		}
	}
	return n, nil
}

// principalSpent reads the principal's use of the day, spent plus reserved, as admission does
// (R6, C8): the request's whole ceiling must still fit under its dailyUSD.
func (s *Server) principalSpent(ctx context.Context, sp *config.Sponsored, maxUSD int64) (bool, error) {
	used, err := DayUsed(ctx, s.Rooms, s.Namespace, sp.Principal(), s.Now())
	if err != nil {
		return false, err
	}
	return used+maxUSD > config.MicroUSD(sp.DailyUSD), nil
}

// admitTask creates the Task before the issue, as RunLore does: the name is the key's, so a racing
// replay loses on AlreadyExists and files no second issue (R12).
func (s *Server) admitTask(ctx context.Context, sp *config.Sponsored, login, name, key, title, text string,
	in TaskRequest, maxUSD int64) (int, map[string]any, error) {
	var filer IssueFiler
	if title != "" && s.Forges != nil {
		if filer = s.Forges(in.Repository); filer == nil {
			return 0, nil, errNoForge // before the Task: a Task that can never get its issue is not created
		}
	}
	snapshot := text
	if title != "" {
		snapshot = "# " + title + "\n\n" + text
	}
	sum := sha256.Sum256([]byte(snapshot))
	clean, _ := sanitize.Text(snapshot)
	ref := in.Ref
	if ref == "" {
		ref = "api:" + sp.Name
	}
	t := &factoryv1.Task{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace},
		Spec: factoryv1.TaskSpec{
			Source: factoryv1.Source{Kind: "api", Ref: ref, Key: "api:" + sp.Name + ":" + key, RequestedBy: sp.Principal(),
				Sponsor: factoryv1.Person(login), OnBehalfOf: in.OnBehalfOf, Trust: "untrusted", ContentSHA256: hex.EncodeToString(sum[:])},
			Repository: in.Repository, Text: clean, DataClass: in.DataClass, MaxMicroUSD: maxUSD, IssueAuthor: login}}
	if err := s.Rooms.Create(ctx, t); apierrors.IsAlreadyExists(err) {
		got, _, err := s.existing(ctx, name)
		if err != nil {
			return 0, nil, err
		}
		if !replayOf(got, sp, key) {
			return 0, nil, errReplayConflict
		}
		return http.StatusOK, map[string]any{"task": name, "room": name, "issue": got.Spec.Issue, "duplicate": true}, nil
	} else if err != nil {
		return 0, nil, err
	}
	body := map[string]any{"task": name, "room": name}
	if filer == nil {
		return http.StatusCreated, body, nil
	}
	footer := fmt.Sprintf("\n\n---\nFiled by the agent factory, sponsored by @%s through %s. Its room: %s/r/%s (tailnet only).",
		login, sp.Principal(), strings.TrimSuffix(s.Cfg.RoomsURL, "/"), name)
	// Past the Task, a client that hangs up must not orphan the issue: filing and linking outlive ctx.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	iss, err := filer.CreateIssue(fctx, title, text+footer, nil) // no label (R10)
	if err != nil {
		return 0, nil, err // the Task exists; a replay finds it and files nothing, so the owner sees it without an issue
	}
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"issue": iss.Number}})
	for attempt := 1; ; attempt++ {
		err = s.Rooms.Patch(fctx, &factoryv1.Task{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace}},
			client.RawPatch(types.MergePatchType, patch))
		if err == nil {
			break
		}
		if !apierrors.IsConflict(err) || attempt == 3 {
			if s.Log != nil { // the issue exists and is unlinked: name it so an operator can link it by hand
				s.Log.ErrorContext(ctx, "intake: the filed issue is not linked to its task", "task", name, "issue", iss.Number, "error", err)
			}
			return 0, nil, err
		}
	}
	body["issue"] = iss.Number
	return http.StatusCreated, body, nil
}
