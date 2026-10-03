// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	factoryv1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/fmetrics"
	"github.com/Smana/agent-platform/internal/factory/runs"
	"github.com/Smana/agent-platform/internal/policy"
)

// Authenticating maps a request to its caller. Task 5.1's Authenticator satisfies it; the
// principal is always the token's, never the body's (SC-13).
type Authenticating interface {
	Authenticate(*http.Request) (authn.Principal, error)
}

// RunStore creates the claim and lists the live runs the room and branch checks read. Since
// R50 the daily budget does not come from List: it is admitted through the day's ledger.
type RunStore interface {
	Create(ctx context.Context, s runs.Spec) error
	List(ctx context.Context) ([]runs.Run, error)
}

// RunRequest is the POST /v1/runs body (§4). Unknown fields — principal and branch included —
// are ignored: the principal is the token's (SC-13) and the branch the factory's (C3).
type RunRequest struct {
	Role       string `json:"role"`
	Repository string `json:"repository"`
	BaseRef    string `json:"baseRef"`
	Task       struct {
		Text string `json:"text"`
		URL  string `json:"url"`
	} `json:"task"`
	DataClass      string   `json:"dataClass"`
	RoomRef        string   `json:"roomRef"`
	Model          string   `json:"model"`
	MaxTokens      int64    `json:"maxTokens"`
	EgressProfiles []string `json:"egressProfiles"`
	// R35: a stopped run's branch, to resume on. Never a task's or a live run's.
	ResumeBranch string `json:"resumeBranch"`
}

// Server is the run-request API (§4). It serves on every replica (NeedLeaderElection is false),
// and admission is atomic across replicas through the day's ledger (R50).
type Server struct {
	Auth Authenticating
	// Groups names the two agents groups: policy's StartRun rule is the room UI's rule, so
	// the CLI grants nothing the UI refuses (SP2 §1).
	Groups policy.Groups
	Cfg    *config.Config
	Runs   RunStore
	// Rooms is the manager's client: it reads Rooms and Tasks (rights, R35), and R50's
	// ledger reservation writes ConfigMaps, so a client.Reader would not do.
	Rooms     client.Client
	Namespace string
	Stopped   func(context.Context) bool // the stop object (§6.1); true on doubt
	NewRunID  func() string
	Now       func() time.Time
	Metrics   *fmetrics.Set
}

var (
	roles  = []string{"implementer", "reviewer", "tester", "triager"}
	models = []string{"agent-default", "tier-light", "tier-standard", "tier-frontier"}

	profiles = []string{"pypi", "npm", "golang", "crates"}
	prURL    = regexp.MustCompile(`^https://github\.com/[^/]+/[^/]+/pull/[0-9]+$`)
)

// Ledger layout (R50). One ConfigMap per UTC day, agent-factory-ledger-<YYYYMMDD>, in the
// factory's namespace: the apiserver's resourceVersion is the transaction, so two replicas
// cannot take one slot.
const (
	ledgerPrefix = "agent-factory-ledger-"
	// ledgerSpent is one JSON map, principal → tokens. It is a map rather than one key per
	// principal because principal ids carry ":", which no ConfigMap key may hold. The meter
	// (Task 5.3) owns this column; admission only reads it.
	ledgerSpent = "spent"
	// ledgerReserved prefixes one key per admitted run — reserved.<runId>, whose C2 id is
	// key-safe — holding {principal, room, maxTokens}. The meter drops the entry when the run
	// is terminal; deleting a run refunds nothing.
	ledgerReserved = "reserved."
	// dayLayout labels the ledger with the UTC day it counts.
	dayLayout = "20060102"
	// ledgerAttempts bounds the conflict-retry loop: steady contention of that depth is
	// failure, not patience.
	ledgerAttempts = 5
)

// reservation is one ledger entry: what the run may spend, on whose behalf and on which room.
type reservation struct {
	Principal string `json:"principal"`
	Room      string `json:"room,omitempty"`
	MaxTokens int64  `json:"maxTokens"`
}

// NeedLeaderElection is false: every replica serves intake.
func (s *Server) NeedLeaderElection() bool { return false }

// Handler is the API's mux: today only POST /v1/runs, admission in §4's order.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/runs", s.createRun)
	return mux
}

// Start serves on api.listen until ctx ends, then drains.
func (s *Server) Start(ctx context.Context) error {
	srv := &http.Server{Addr: s.Cfg.API.Listen, Handler: s.Handler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second) // the drain outlives the root ctx (bridgeapi's idiom)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func refuse(w http.ResponseWriter, code int, reason string) {
	reply(w, code, map[string]string{"error": reason})
}

// createRun admits a run in §4's order — stop object, shape, repository, room, R37's owner
// defaults, R35's resume branch, the day's ledger — then creates it with the factory's own
// id and branch. Refusals name only codes: no claim or principal data echoes back (authn rule).
func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	p, err := s.Auth.Authenticate(r)
	switch {
	case errors.Is(err, authn.ErrForbidden):
		refuse(w, http.StatusForbidden, "not_permitted")
		return
	case err != nil:
		refuse(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	if s.Stopped(r.Context()) { // §6.1: the stop object pauses intake, this API included
		refuse(w, http.StatusServiceUnavailable, "kill_switch")
		return
	}
	var in RunRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		refuse(w, http.StatusBadRequest, "bad_json")
		return
	}
	if reason := validate(in); reason != "" {
		refuse(w, http.StatusBadRequest, reason)
		return
	}
	if !slices.Contains(s.Cfg.API.Repositories, in.Repository) {
		refuse(w, http.StatusForbidden, "repository_not_allowed") // OD-6
		return
	}
	all, err := s.Runs.List(r.Context())
	if err != nil {
		refuse(w, http.StatusServiceUnavailable, "runs_unavailable")
		return
	}
	id := s.NewRunID()
	branch := "agent/" + id
	if in.RoomRef != "" {
		code, reason := s.admitRoom(r.Context(), p, in, all)
		if code != 0 {
			refuse(w, code, reason)
			return
		}
		branch = "agent/" + in.RoomRef // a room's runs share one branch (C3)
	}
	// R37 (owner default): internal data and triagers are the admins'. After the room checks
	// so a member naming the wrong class for a room they cannot enter learns that first, and
	// before the ledger so a refused request reserves nothing. System callers are allowlisted
	// one by one (R23) and are not humans in groups.
	if (in.DataClass == "internal" || in.Role == "triager") && p.Kind == envelope.ActorHuman &&
		!slices.Contains(p.Groups, s.Groups.Admin) {
		refuse(w, http.StatusForbidden, "admin_only")
		return
	}
	if in.ResumeBranch != "" {
		code, reason := s.admitResume(r.Context(), p, in.ResumeBranch, all)
		if code != 0 {
			refuse(w, code, reason)
			return
		}
		branch = in.ResumeBranch
	}
	// The defaults precede the reservation: the ledger must reserve what the run may spend.
	if in.MaxTokens == 0 {
		in.MaxTokens = 2_000_000
	}
	if in.Model == "" {
		in.Model = "agent-default"
	}
	if in.BaseRef == "" {
		in.BaseRef = "main"
	}
	if code, reason := s.reserve(r.Context(), p, id, in); code != 0 {
		refuse(w, code, reason)
		return
	}
	spec := runs.Spec{RunID: id, Role: in.Role, Repository: in.Repository, BaseRef: in.BaseRef, Branch: branch,
		TaskText: in.Task.Text, TaskURL: in.Task.URL, Principal: p.ID, DataClass: in.DataClass, Model: in.Model,
		RoomRef: in.RoomRef, Queue: runs.QueueInteractive, MaxTokens: in.MaxTokens, MaxMinutes: 120,
		EgressProfiles: in.EgressProfiles}
	if err := s.Runs.Create(r.Context(), spec); err != nil {
		// Without this the reservation would eat the principal's day (and room) forever:
		// the meter drops entries only of runs that reach a terminal phase, and this one
		// never existed. Best effort — a leaked entry stays bounded by the day's ledger.
		s.dropReservation(r.Context(), id)
		refuse(w, http.StatusServiceUnavailable, "create_failed")
		return
	}
	reply(w, http.StatusCreated, map[string]string{"runId": id, "branch": branch})
}

func validate(in RunRequest) string {
	switch {
	case !slices.Contains(roles, in.Role):
		return "bad_role"
	case (in.Task.Text == "") == (in.Task.URL == ""):
		return "exactly_one_of_task_text_or_url"
	case len(in.Task.Text) > 16384:
		return "task_text_too_long"
	case (in.Role == "reviewer" || in.Role == "tester") && !prURL.MatchString(in.Task.URL):
		return "reviewer_needs_a_pull_request_url"
	case in.DataClass != "public" && in.DataClass != "internal":
		return "bad_data_class"
	case in.RoomRef != "" && !envelope.ValidID(in.RoomRef):
		return "bad_room"
	case in.ResumeBranch != "" && !resumeBranchValid(in.ResumeBranch):
		return "bad_resume_branch"
	case in.ResumeBranch != "" && in.RoomRef != "":
		return "a_room_already_names_its_branch"
	case in.Model != "" && !slices.Contains(models, in.Model):
		return "bad_model"
	case in.MaxTokens < 0 || in.MaxTokens > config.RunTokenCeiling:
		return "max_tokens_out_of_range"
	case len(in.EgressProfiles) > 4:
		return "too_many_egress_profiles"
	}
	for _, e := range in.EgressProfiles {
		if !slices.Contains(profiles, e) {
			return "bad_egress_profile"
		}
	}
	return ""
}

// resumeBranchValid is R35's shape: agent/ followed by the C2 id of the stopped run.
func resumeBranchValid(b string) bool {
	id, ok := strings.CutPrefix(b, "agent/")
	return ok && envelope.ValidID(id)
}

// admitRoom: the room exists, its data class matches, the caller may start a run in it by SP2's
// own rule, and it has no live run (SP2 §1: one Running run per room). Cross-replica exclusion
// is the ledger's (R50).
func (s *Server) admitRoom(ctx context.Context, p authn.Principal, in RunRequest, all []runs.Run) (int, string) {
	var room v1alpha1.Room
	err := s.Rooms.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: in.RoomRef}, &room)
	if apierrors.IsNotFound(err) {
		return http.StatusBadRequest, "no_room"
	}
	if err != nil {
		return http.StatusServiceUnavailable, "rooms_unavailable"
	}
	if room.Spec.DataClass != in.DataClass {
		return http.StatusBadRequest, "data_class_differs_from_the_room"
	}
	// The broker's rule, not a copy of it: the driver or the owner (agents-admin owns every room).
	// StartRun is not UI-only (SP2 P18), so webUI does not change the answer.
	if !policy.Allowed(s.Groups.Resolve(&room, p, room.Status.Driver, false), policy.StartRun) {
		return http.StatusForbidden, "not_permitted"
	}
	for _, x := range all {
		if x.RoomRef == in.RoomRef && !runs.Terminal(x.Phase) {
			return http.StatusConflict, "room_busy"
		}
	}
	return 0, ""
}

// admitResume is R35's one caller-named branch: a stopped run's agent/<id>. A task's branch
// resumes with /factory retry, a room's needs the caller's right to start runs there, and a
// branch a live run holds is busy. The branch widens nothing: the agents' App may push any
// agent/** branch already; SC-14's run-reported head and SHA-bound verdicts (R52) keep foreign
// commits away from the merge.
func (s *Server) admitResume(ctx context.Context, p authn.Principal, branch string, all []runs.Run) (int, string) {
	id := strings.TrimPrefix(branch, "agent/")
	var task factoryv1.Task
	switch err := s.Rooms.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: id}, &task); {
	case err == nil:
		return http.StatusForbidden, "task_branch"
	case !apierrors.IsNotFound(err):
		return http.StatusServiceUnavailable, "tasks_unavailable"
	}
	var room v1alpha1.Room
	switch err := s.Rooms.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: id}, &room); {
	case err == nil:
		if !policy.Allowed(s.Groups.Resolve(&room, p, room.Status.Driver, false), policy.StartRun) {
			return http.StatusForbidden, "not_permitted"
		}
	case !apierrors.IsNotFound(err):
		return http.StatusServiceUnavailable, "rooms_unavailable"
	}
	for _, x := range all {
		if x.Branch == branch && !runs.Terminal(x.Phase) {
			return http.StatusConflict, "branch_busy"
		}
	}
	return 0, ""
}

// reserve is R50's atomic admission, replacing admitBudget's live-run sum: read the day's
// ledger, check spent + reserved + maxTokens against the cap and that no reservation holds the
// room, then write reserved.<runId> — the Update conflicts against a concurrent replica's,
// and the retry re-reads and re-checks, so exactly one admission per slot lands whatever the
// interleaving. Shadow mode (R3) counts but does not refuse; room exclusion stays exact.
func (s *Server) reserve(ctx context.Context, p authn.Principal, id string, in RunRequest) (int, string) {
	limit := s.Cfg.Budgets.HumanDaily
	if p.ID == runs.PrincipalFactory {
		limit = s.Cfg.Budgets.FactoryDaily
	}
	name := ledgerPrefix + s.Now().UTC().Format(dayLayout)
	key := types.NamespacedName{Namespace: s.Namespace, Name: name}
	mine := reservation{Principal: p.ID, Room: in.RoomRef, MaxTokens: in.MaxTokens}
	for range ledgerAttempts {
		cm := &corev1.ConfigMap{}
		err := s.Rooms.Get(ctx, key, cm)
		existed := true
		if apierrors.IsNotFound(err) {
			cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace}}
			existed = false
		} else if err != nil {
			return http.StatusServiceUnavailable, "ledger_unavailable"
		}
		spent, err := ledgerSpentColumn(cm)
		if err != nil {
			return http.StatusServiceUnavailable, "ledger_unavailable"
		}
		reserved, err := ledgerReservations(cm)
		if err != nil {
			return http.StatusServiceUnavailable, "ledger_unavailable"
		}
		// R48: the run id is the retry's identity. Our own equal entry means we already
		// landed this admission: pass without reserving twice. A different value under this
		// id is another run's slot, and overwriting it would unreserve theirs: refuse.
		if prev, ok := reserved[id]; ok {
			if prev == mine {
				return 0, ""
			}
			return http.StatusConflict, "run_busy"
		}
		used := spent[p.ID]
		for _, v := range reserved {
			if v.Principal == p.ID {
				used += v.MaxTokens
			}
		}
		over := used+in.MaxTokens > limit
		if over && s.Cfg.Budgets.EnforcePrincipal {
			return http.StatusTooManyRequests, "over_budget"
		}
		if in.RoomRef != "" {
			for _, v := range reserved {
				if v.Room == in.RoomRef {
					return http.StatusConflict, "room_busy"
				}
			}
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		b, err := json.Marshal(mine)
		if err != nil {
			return http.StatusServiceUnavailable, "ledger_unavailable"
		}
		cm.Data[ledgerReserved+id] = string(b)
		if existed {
			err = s.Rooms.Update(ctx, cm)
		} else {
			err = s.Rooms.Create(ctx, cm) // a concurrent Create conflicts; the loop re-reads
		}
		switch {
		case err == nil:
			if over {
				s.Metrics.Revoked(ctx, "budget-principal-shadow") // R3: counted, not enforced
			}
			return 0, ""
		case apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err):
			continue // re-read and re-check (R50)
		default:
			return http.StatusServiceUnavailable, "ledger_unavailable"
		}
	}
	// Conflicts at this depth mean steady contention, not a lost slot: fail open never.
	return http.StatusServiceUnavailable, "ledger_busy"
}

// dropReservation undoes reserve when the run creation itself failed. Best effort: a leaked
// entry costs one day of budget, while a delete that fights a concurrent replica could drop
// another admission's reservation instead.
func (s *Server) dropReservation(ctx context.Context, id string) {
	key := types.NamespacedName{Namespace: s.Namespace, Name: ledgerPrefix + s.Now().UTC().Format(dayLayout)}
	cm := &corev1.ConfigMap{}
	if err := s.Rooms.Get(ctx, key, cm); err != nil {
		return
	}
	if _, ok := cm.Data[ledgerReserved+id]; !ok {
		return
	}
	delete(cm.Data, ledgerReserved+id)
	_ = s.Rooms.Update(ctx, cm)
}

func ledgerSpentColumn(cm *corev1.ConfigMap) (map[string]int64, error) {
	raw, ok := cm.Data[ledgerSpent]
	if !ok || raw == "" {
		return nil, nil
	}
	var m map[string]int64
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("ledger %s: %w", cm.Name, err)
	}
	return m, nil
}

func ledgerReservations(cm *corev1.ConfigMap) (map[string]reservation, error) {
	m := map[string]reservation{}
	for k, raw := range cm.Data {
		id, ok := strings.CutPrefix(k, ledgerReserved)
		if !ok {
			continue
		}
		var r reservation
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, fmt.Errorf("ledger %s reservation %s: %w", cm.Name, id, err)
		}
		m[id] = r
	}
	return m, nil
}
