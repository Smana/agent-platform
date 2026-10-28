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
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	factoryv1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/authn"
	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/fmetrics"
	"github.com/Smana/maku/internal/factory/forge"
	"github.com/Smana/maku/internal/factory/sanitize"
	"github.com/Smana/maku/internal/factory/taskid"
	"github.com/Smana/maku/internal/ghidentity"
	"github.com/Smana/maku/internal/sponsored"
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
	// Incident is an internal principal's finding (RA3); a sponsored principal's request refuses it.
	Incident *Incident `json:"incident,omitempty"`
}

// Incident is RunLore's finding, as an internal principal sends it (RA3, RA6): identifiers for the
// stub issue and the gates' inputs. The finding itself is the request's text.
type Incident struct {
	Alert      string  `json:"alert"`
	Resource   string  `json:"resource"`
	Severity   string  `json:"severity"`
	Cluster    string  `json:"cluster"`
	Verdict    string  `json:"verdict"`
	Confidence float64 `json:"confidence"`
}

const (
	reasonSponsoredOnly = "sponsored_only"
	reasonPrincipalOver = "principal_budget_exhausted"
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
	if ip := s.Cfg.InternalBy(p.ID); p.Internal && ip != nil {
		s.createInternalTask(w, r, p, ip)
		return
	}
	sp := s.Cfg.SponsoredBy(p.ID)
	if !p.Sponsored || p.Internal || sp == nil {
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
	case in.Incident != nil: // an internal principal's field: a sponsored request has no use for it
		out(http.StatusBadRequest, "bad_json")
		return
	case key == "" || len(key) > maxKeyLen:
		out(http.StatusBadRequest, "idempotency_key_required")
		return
	case in.DataClass != "public": // R37: internal data is the admins', and a machine is none
		out(http.StatusBadRequest, "bad_data_class")
		return
	case !sponsored.ValidRef(in.Ref): // the ref is written into the trusted brief: one pattern with the broker
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
	login, code, reason := s.sponsorOf(ctx, sp, in)
	if reason != "" {
		out(code, reason)
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

// reasonLinkUnverified is a self-serve link the broker or ZITADEL could not answer for: a 503.
const reasonLinkUnverified = "link_unverified"

// sponsorOf is the request's sponsor login (R1, R3, R9): a linked user's maintainer, else the
// config's sponsor, re-checked as a maintainer of the repository either way. A Slack user is
// linked by onBehalfOf.slack first (the config wins), else, for a principal with
// onBehalfOf.slackLinks, by the broker's self-serve link record: its ZITADEL sub's current GitHub
// login, never a login the caller sent.
func (s *Server) sponsorOf(ctx context.Context, sp *config.Sponsored, in TaskRequest) (string, int, string) {
	login := sp.Sponsor
	if in.OnBehalfOf != "" {
		id, found := strings.CutPrefix(in.OnBehalfOf, "slack:")
		if !found {
			return "", http.StatusForbidden, "not_linked" // only slack:<team>/<user> links exist
		}
		var ok bool
		if login, ok = sp.OnBehalfOf.Slack[id]; !ok {
			var code int
			var reason string
			if login, code, reason = s.selfServeLogin(ctx, sp, id, in.Repository); reason != "" {
				return "", code, reason
			}
		}
	}
	if !s.Cfg.RepositoryFor(in.Repository).IsMaintainer(login) {
		return "", http.StatusForbidden, "not_a_maintainer"
	}
	return strings.ToLower(login), 0, ""
}

// selfServeLogin is the GitHub login a Slack user's self-serve link resolves to on repository.
// No link, an inactive account or an account with no GitHub link is not_linked; a lookup that
// fails is link_unverified, never a guess.
func (s *Server) selfServeLogin(ctx context.Context, sp *config.Sponsored, id, repository string) (string, int, string) {
	team, user, _ := strings.Cut(id, "/")
	if !sp.OnBehalfOf.SlackLinks || s.SlackLinks == nil || s.Logins == nil {
		return "", http.StatusForbidden, "not_linked"
	}
	sub, found, err := s.SlackLinks.SlackLink(ctx, team, user)
	switch {
	case err != nil:
		s.logLink(ctx, "the broker's slack link is unreadable", err)
		return "", http.StatusServiceUnavailable, reasonLinkUnverified
	case !found:
		return "", http.StatusForbidden, "not_linked"
	}
	login, err := s.Logins.Login(ctx, sub, repository)
	switch {
	case errors.Is(err, ghidentity.ErrInactive):
		return "", http.StatusForbidden, "not_linked"
	case err != nil:
		s.logLink(ctx, "the linked account's GitHub login is unreadable", err)
		return "", http.StatusServiceUnavailable, reasonLinkUnverified
	case login == "":
		return "", http.StatusForbidden, "not_linked"
	}
	return login, 0, ""
}

func (s *Server) logLink(ctx context.Context, msg string, err error) {
	if s.Log != nil {
		s.Log.WarnContext(ctx, "intake: "+msg, "error", err)
	}
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
	body := map[string]any{"task": name, "room": name, "sponsor": login}
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
	if err := s.linkIssue(fctx, name, iss.Number, false); err != nil {
		return 0, nil, err
	}
	body["issue"] = iss.Number
	return http.StatusCreated, body, nil
}

// linkIssue records the filed issue on the Task, three attempts on conflict. A failure names the
// unlinked issue so an operator can link it by hand.
//
// labelIssue also sets the task's issue label, which the issue poller finds a task by: an internal
// principal's stub issue is where factory/publish and factory/stop are read. A sponsored task keeps
// no label, so a factory/ready on its issue never meets it as an active task.
func (s *Server) linkIssue(ctx context.Context, name string, issue int, labelIssue bool) error {
	doc := map[string]any{"spec": map[string]any{"issue": issue}}
	if labelIssue {
		doc["metadata"] = map[string]any{"labels": map[string]string{factoryv1.LabelIssue: strconv.Itoa(issue)}}
	}
	patch, _ := json.Marshal(doc)
	for attempt := 1; ; attempt++ {
		err := s.Rooms.Patch(ctx, &factoryv1.Task{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace}},
			client.RawPatch(types.MergePatchType, patch))
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) || attempt == 3 {
			if s.Log != nil {
				s.Log.ErrorContext(ctx, "intake: the filed issue is not linked to its task", "task", name, "issue", issue, "error", err)
			}
			return err
		}
	}
}
