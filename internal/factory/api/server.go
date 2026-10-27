// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	// Owned is v2 D7's fence: whether the repository's default branch names this factory. nil owns
	// nothing.
	Owned func(repository string) bool
	// Access is v2 D4: whether a human may write the repository on GitHub. nil refuses every
	// human: GitHub is the one answer to who may change a repository, and there is no bypass.
	Access WriteChecker
	// People is D1's identity link, read only while budgets.perPerson is on: the person a human
	// token is charged as. nil charges every human as their own principal.
	People PersonResolver
	// Forges files an api task's issue as the factory App on its repository (R10): nil files none;
	// a nil filer for a repository refuses a titled request.
	Forges func(repository string) IssueFiler
	// Log records the causes the intake never answers; nil records none.
	Log *slog.Logger
	// Redact is the broker's secret redactor (*redact.Redactor), applied to an api task's issue
	// before it is filed; nil refuses a titled request.
	Redact interface {
		String(s string) (string, []string)
	}
}

// PersonResolver names the person a human principal is charged as (v2 per-person caps D1):
// github:<login> through their ZITADEL link, "" with no link. An error is no answer.
type PersonResolver interface {
	Person(ctx context.Context, p authn.Principal, repository string) (string, error)
}

// WriteChecker answers v2 D4 for a human principal: write or higher on repository, from GitHub.
// An error is no answer, and the caller refuses.
type WriteChecker interface {
	CanWrite(ctx context.Context, p authn.Principal, repository string) (bool, error)
}

var (
	roles  = []string{"implementer", "reviewer", "tester", "triager"}
	models = []string{"agent-default", "tier-light", "tier-standard", "tier-frontier"}

	prURL = regexp.MustCompile(`^https://github\.com/[^/]+/[^/]+/pull/[0-9]+$`)
)

// Ledger layout (R50). One ConfigMap per UTC day, agent-factory-ledger-usd-<YYYYMMDD>, in the
// factory's namespace: the apiserver's resourceVersion is the transaction, so two replicas
// cannot take one slot. Every amount is micro-USD (truthful costs TC6). Exported so the meter
// reads and writes the same encoding: admission owns the reservations, the meter owns the spend.
const (
	LedgerPrefix = "agent-factory-ledger-usd-"
	// LegacyLedgerPrefix names the token ledgers the USD ones replaced (#155): nothing reads or
	// writes them; the meter's pruner deletes them once they are older than its retention.
	LegacyLedgerPrefix = "agent-factory-ledger-"
	// LedgerSpent is one JSON map, principal → micro-USD. It is a map rather than one key per
	// principal because principal ids carry ":", which no ConfigMap key may hold. The meter
	// owns this column; admission only reads it.
	LedgerSpent = "spent"
	// LedgerReserved prefixes one key per admitted run — reserved.<runId>, whose C2 id is
	// key-safe — holding {principal, person, room, maxMicroUSD}. Release drops the entry when the
	// run ends or its claim goes (#55); the spend it made stays: deleting a run refunds nothing.
	LedgerReserved = "reserved."
	// LedgerSettled prefixes one key per metered run, settled.<runId>: the micro-USD charged to
	// that run that day, written by the meter in the same write as spent. Per key, spent is the
	// sum of settled over the runs charged to it; summed over the retained days, it is how much
	// of a run any meter has charged, so a new leader charges neither twice nor less (TC6). One
	// exception: on the day a lifted run (R10) is first charged, its settled entry also holds the
	// lift, which spent never counted, so settled exceeds that run's share of spent by it.
	LedgerSettled = "settled."
	// LedgerDayLayout labels the ledger with the UTC day it counts.
	LedgerDayLayout = "20060102"
	// LedgerAttempts bounds the conflict-retry loop: steady contention of that depth is
	// failure, not patience.
	LedgerAttempts = 5
)

// Reservation is one ledger entry: what the run may spend, in micro-USD, on whose behalf and on
// which room. Person is the second key the same entry charges (v2 per-person caps D3): the person
// who caused the run, beside the fleet as Principal. One key per run is what makes both
// reservations one write: no run ever holds one without the other.
type Reservation struct {
	Principal   string `json:"principal"`
	Person      string `json:"person,omitempty"`
	Room        string `json:"room,omitempty"`
	MaxMicroUSD int64  `json:"maxMicroUSD"`
}

// NeedLeaderElection is false: every replica serves intake.
func (s *Server) NeedLeaderElection() bool { return false }

// Handler is the API's mux: POST /v1/runs, admission in §4's order, and POST /v1/tasks while a
// sponsored principal is declared.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/runs", s.createRun)
	if len(s.Cfg.API.Sponsored)+len(s.Cfg.API.InternalPrincipals) > 0 { // F4, D4: off until a principal is declared
		mux.HandleFunc("POST /v1/tasks", s.createTask)
	}
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

// refuseRepository refuses and counts a refusal by repository (v2 observability): the counter
// keeps only the repository codes, so its label stays bounded.
func (s *Server) refuseRepository(ctx context.Context, w http.ResponseWriter, code int, reason string) {
	if s.Metrics != nil {
		s.Metrics.AdmissionRefused(ctx, reason)
	}
	refuse(w, code, reason)
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
	if p.Sponsored { // F4: a machine starts work only as a Task, never as a bare run
		refuse(w, http.StatusForbidden, "sponsored_only_tasks")
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
	// Exact, not case-insensitive: spec.repository becomes the run's octo-sts audience, so only
	// the config's spelling is ever written there.
	if !slices.Contains(s.Cfg.API.Repositories, in.Repository) {
		refuse(w, http.StatusForbidden, "repository_not_allowed") // OD-6
		return
	}
	// Spend per identity SI4: a refusal names the requester as authenticated. Resolving their
	// GitHub link here would cost the call mayWrite saves for a request every local rule admits.
	ctx := fmetrics.WithPrincipal(fmetrics.WithRepository(r.Context(), in.Repository), p.ID)
	if s.Owned == nil || !s.Owned(in.Repository) { // v2 D7: another factory's, or nobody's
		s.refuseRepository(ctx, w, http.StatusConflict, "repository_not_owned")
		return
	}
	// The defaults come first: the request's fingerprint and the ledger's reservation both
	// carry what the run will get, not what the caller left out.
	if in.MaxTokens == 0 {
		in.MaxTokens = 2_000_000
	}
	if in.Model == "" {
		in.Model = "agent-default"
	}
	if in.BaseRef == "" {
		in.BaseRef = "main"
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) > maxKeyLen {
		refuse(w, http.StatusBadRequest, "bad_idempotency_key")
		return
	}
	all, err := s.Runs.List(r.Context())
	if err != nil {
		refuse(w, http.StatusServiceUnavailable, "runs_unavailable")
		return
	}
	hash := requestHash(in)
	id := ""
	if key != "" {
		var first *runs.Run
		var collision bool
		id, first, collision = s.keyedRun(p, key, all)
		if collision {
			refuse(w, http.StatusConflict, "idempotency_key_collision")
			return
		}
		if first != nil {
			// The room, branch and ledger checks below would meet this run's own claim and
			// reservation as busy: a retry is answered before them, with the first answer.
			if first.RequestHash != hash {
				refuse(w, http.StatusUnprocessableEntity, "idempotency_key_reused")
				return
			}
			w.Header().Set("Idempotent-Replayed", "true")
			reply(w, http.StatusCreated, map[string]string{"runId": first.ID, "branch": first.Branch})
			return
		}
	} else {
		id = s.NewRunID()
	}
	branch := "agent/" + id
	suspendable := false
	if in.RoomRef != "" {
		code, reason, attended := s.admitRoom(r.Context(), p, in, all)
		if code != 0 {
			s.refuseRepository(ctx, w, code, reason)
			return
		}
		branch = "agent/" + in.RoomRef // a room's runs share one branch (C3)
		// v2 suspend: only an attended room has a human whose approval a run may wait on.
		suspendable = attended && s.Cfg.Sandbox.Suspend.Enabled
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
		code, reason := s.admitResume(r.Context(), p, in.ResumeBranch, in.Repository, all)
		if code != 0 {
			s.refuseRepository(ctx, w, code, reason)
			return
		}
		branch = in.ResumeBranch
	}
	// v2 D4/D6, last before the ledger: the one check that asks GitHub, made only for a request
	// every local rule admitted, so a refused one reserves nothing and costs no call.
	if !s.mayWrite(r.Context(), p, in.Repository) {
		s.refuseRepository(ctx, w, http.StatusForbidden, "repository_permission")
		return
	}
	person, err := s.person(r.Context(), p, in.Repository)
	if err != nil {
		refuse(w, http.StatusServiceUnavailable, "identity_unavailable")
		return
	}
	if code, reason := s.reserve(ctx, p, person, id, in); code != 0 {
		if reason == reasonPersonOver {
			s.refuseRepository(fmetrics.WithPrincipal(ctx, person), w, code, reason) // the key over, as the shadow names it
			return
		}
		refuse(w, code, reason)
		return
	}
	spec := runs.Spec{RunID: id, Role: in.Role, Repository: in.Repository, BaseRef: in.BaseRef, Branch: branch,
		TaskText: in.Task.Text, TaskURL: in.Task.URL, Principal: p.ID, ChargedTo: person, DataClass: in.DataClass, Model: in.Model,
		RoomRef: in.RoomRef, Queue: runs.QueueInteractive, MaxTokens: in.MaxTokens, MaxMinutes: 120,
		EgressProfiles: in.EgressProfiles, RequestHash: hash, Suspendable: suspendable}
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

// maxKeyLen bounds an Idempotency-Key; the broker's is <room>:<client>:<seq>, well under it.
const maxKeyLen = 255

// KeyLifetime is how long an Idempotency-Key answers with its first run: the ledger's day.
const KeyLifetime = 24 * time.Hour

// keyedRun derives the run id of (principal, key) and finds the run already made under it. The
// id is a hash, so a retry on any replica names the same claim, and the ledger's equal-entry
// pass (R48) holds the one reservation. The UTC day is in the hash, so a key frees itself when
// the day turns; the previous day's id is looked up too, so a retry across midnight still finds
// a run younger than KeyLifetime. first is nil when the key has no live run, and id is then the
// one to create.
//
// The id is 40 bits and run ids are public (agent/<id> branches), so a claim under a candidate id
// that another principal owns is never a replay: collision is true and the caller refuses with 409
// (creating would collide on the claim name anyway). Known gap: concurrent retries of one key
// either side of 00:00 UTC both find nothing and compute different ids, so they make two runs.
func (s *Server) keyedRun(p authn.Principal, key string, all []runs.Run) (id string, first *runs.Run, collision bool) {
	now := s.Now()
	id = keyedID(now, p.ID, key)
	for _, cand := range []string{id, keyedID(now.Add(-KeyLifetime), p.ID, key)} {
		for i := range all {
			switch {
			case all[i].ID != cand:
			case all[i].Principal != p.ID:
				return id, nil, true
			case now.Sub(all[i].Created) < KeyLifetime:
				return cand, &all[i], false
			}
		}
	}
	return id, nil, false
}

func keyedID(at time.Time, principal, key string) string {
	sum := sha256.Sum256([]byte(at.UTC().Format(LedgerDayLayout) + "\x00" + principal + "\x00" + key))
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:5]))
}

// requestHash fingerprints the admitted request, defaults applied: a replay with the same key
// and another fingerprint is a different request, not a retry. Egress profiles are a set (sorted,
// nil equals empty). It hashes every RunRequest field, so adding one changes the hash of a run
// made before the deploy: a retry across that rollout may get 422 once.
func requestHash(in RunRequest) string {
	in.EgressProfiles = slices.Sorted(slices.Values(in.EgressProfiles))
	if len(in.EgressProfiles) == 0 {
		in.EgressProfiles = nil
	}
	b, _ := json.Marshal(in) // plain strings, numbers and slices: Marshal cannot fail
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
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
	// T4/T5 (identity design §2, §7): no direct GitHub or profile egress for internal data. The
	// AgentRun XRD refuses the claim too; this names the reason before anything is reserved.
	case in.DataClass == "internal" && len(in.EgressProfiles) > 0:
		return "internal_runs_take_no_egress_profiles"
	}
	for _, e := range in.EgressProfiles {
		if !slices.Contains(config.EgressProfiles, e) {
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
// is the ledger's (R50). attended reports an admitted room's approval profile.
func (s *Server) admitRoom(ctx context.Context, p authn.Principal, in RunRequest, all []runs.Run) (code int, reason string, attended bool) {
	var room v1alpha1.Room
	err := s.Rooms.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: in.RoomRef}, &room)
	if apierrors.IsNotFound(err) {
		return http.StatusBadRequest, "no_room", false
	}
	if err != nil {
		return http.StatusServiceUnavailable, "rooms_unavailable", false
	}
	if room.Spec.DataClass != in.DataClass {
		return http.StatusBadRequest, "data_class_differs_from_the_room", false
	}
	// v2 D3: the room's repository wins, never overwritten silently (the fingerprint would hash a
	// body the caller never sent). A room without one binds to none.
	if !sameRepository(room.Spec.Repository, in.Repository) {
		return http.StatusBadRequest, "repository_differs_from_the_room", false
	}
	// The broker's rule, not a copy of it: the driver or the owner (agents-admin owns every room).
	// StartRun is not UI-only (SP2 P18), so webUI does not change the answer.
	if !policy.Allowed(s.Groups.Resolve(&room, p, room.Status.Driver, false), policy.StartRun) {
		return http.StatusForbidden, "not_permitted", false
	}
	for _, x := range all {
		if x.RoomRef == in.RoomRef && !runs.Terminal(x.Phase) {
			return http.StatusConflict, "room_busy", false
		}
	}
	return 0, "", room.Spec.Approvals.Profile != "unattended"
}

// admitResume is R35's one caller-named branch: a stopped run's agent/<id>. A task's branch
// resumes with /factory retry, a room's needs the caller's right to start runs there, and a
// branch a live run holds is busy. The branch widens nothing: the agents' App may push any
// agent/** branch already; SC-14's run-reported head and SHA-bound verdicts (R52) keep foreign
// commits away from the merge.
//
// v2 D3: the branch keeps its repository, the one its newest run or its room names. A branch
// neither names is bound by D4 alone: the caller must write the repository they asked for.
func (s *Server) admitResume(ctx context.Context, p authn.Principal, branch, repository string, all []runs.Run) (int, string) {
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
		if !sameRepository(room.Spec.Repository, repository) {
			return http.StatusBadRequest, "repository_differs_from_the_branch"
		}
	case !apierrors.IsNotFound(err):
		return http.StatusServiceUnavailable, "rooms_unavailable"
	}
	var last *runs.Run
	for i, x := range all {
		if x.Branch != branch {
			continue
		}
		if !runs.Terminal(x.Phase) {
			return http.StatusConflict, "branch_busy"
		}
		if last == nil || x.Created.After(last.Created) {
			last = &all[i]
		}
	}
	if last != nil && !sameRepository(last.Repository, repository) {
		return http.StatusBadRequest, "repository_differs_from_the_branch"
	}
	return 0, ""
}

// sameRepository compares owner/name as GitHub does, case-insensitively. An empty bound names no
// repository, so it matches none.
func sameRepository(bound, asked string) bool {
	return bound != "" && strings.EqualFold(bound, asked)
}

// mayWrite is v2 D4 for a human — write or higher on GitHub, admins included — and D6 for a system
// caller: the repositories api.systemRepositories lists for it. Any doubt refuses.
func (s *Server) mayWrite(ctx context.Context, p authn.Principal, repository string) bool {
	if p.Kind != envelope.ActorHuman {
		return slices.ContainsFunc(s.Cfg.API.SystemRepositories[p.ID], func(r string) bool { return strings.EqualFold(r, repository) })
	}
	if s.Access == nil {
		return false
	}
	ok, err := s.Access.CanWrite(ctx, p, repository)
	return err == nil && ok
}

// reasonPersonOver is the API's refusal of a person past their day (D4), counted by repository.
const reasonPersonOver = "person_budget_exhausted"

// person is who a human's run is charged to while per-person caps are on (D1): their GitHub
// login, else their own principal; "" when the caps are off or the caller is a system.
func (s *Server) person(ctx context.Context, p authn.Principal, repository string) (string, error) {
	if !s.Cfg.Budgets.PerPerson.Enabled || p.Kind != envelope.ActorHuman {
		return "", nil
	}
	if s.People == nil {
		return p.ID, nil
	}
	who, err := s.People.Person(ctx, p, repository)
	if err != nil {
		return "", err
	}
	if who == "" {
		return p.ID, nil // no link: the sub is the person, and cannot label anyway (D1)
	}
	return who, nil
}

// reserve is R50's admission on the day's ledger (Reserve), its refusals said in HTTP. A run
// charged to a person reserves the fleet and the person in one entry (D2, D3). A run request caps
// in tokens, so it reserves its maxTokens at the reference rate until F4 brings max_cost (TC7).
func (s *Server) reserve(ctx context.Context, p authn.Principal, person, id string, in RunRequest) (int, string) {
	b := s.Cfg.Budgets
	mine := Reservation{Principal: p.ID, Room: in.RoomRef, MaxMicroUSD: config.RefMicroUSD(in.MaxTokens)}
	if person != "" {
		mine.Principal, mine.Person = runs.PrincipalFactory, person
	}
	over, err := Reserve(ctx, s.Rooms, s.Namespace, s.Now(), id, mine, b.DailyMicroUSD(mine.Principal), b.DailyMicroUSD(person), b.EnforcePrincipal)
	switch {
	case errors.Is(err, ErrPersonOverBudget):
		return http.StatusTooManyRequests, reasonPersonOver
	case errors.Is(err, ErrOverBudget):
		return http.StatusTooManyRequests, "over_budget"
	case errors.Is(err, ErrRunBusy):
		return http.StatusConflict, "run_busy"
	case errors.Is(err, ErrRoomBusy):
		return http.StatusConflict, "room_busy"
	case errors.Is(err, ErrLedgerBusy):
		return http.StatusServiceUnavailable, "ledger_busy"
	case errors.Is(err, ErrMeterBlind):
		return http.StatusServiceUnavailable, "meter_blind"
	case err != nil:
		return http.StatusServiceUnavailable, "ledger_unavailable"
	}
	if over != "" {
		s.Metrics.PrincipalShadow(ctx, over) // R3: counted, not enforced
	}
	return 0, ""
}

// Reserve's refusals. Any other error is the ledger unread or unwritten.
var (
	ErrOverBudget = errors.New("ledger: over the daily budget")
	// ErrPersonOverBudget is the person's day, not the fleet's (D4): checked first, so a refusal
	// names whose cap it is.
	ErrPersonOverBudget = errors.New("ledger: over the person's daily budget")
	ErrRunBusy          = errors.New("ledger: the run id holds another reservation")
	ErrRoomBusy         = errors.New("ledger: a reservation holds the room")
	ErrLedgerBusy       = errors.New("ledger: busy")
)

// Reserve is R50's atomic admission, shared by the API and the factory's Queued (#78): read the
// UTC day of now's ledger, check spent + reserved + maxMicroUSD against limit and that no
// reservation holds mine.Room, then write reserved.<id> — the Update conflicts against a
// concurrent writer's, and the retry re-reads and re-checks, so exactly one admission per slot
// lands whatever the interleaving. Unless enforce (R3's shadow), an over day is reported as over,
// written, and never refused; room exclusion stays exact. An enforced cap also refuses while the
// meter is blind (#54): its spend column is then stale. Release drops the entry when the run ends.
//
// mine.Person, when set, is checked against personLimit in the same read and charged by the same
// write (D3). over names the key whose day the run does not fit, the person's first; "" fits.
func Reserve(ctx context.Context, c client.Client, ns string, now time.Time, id string, mine Reservation,
	limit, personLimit int64, enforce bool) (over string, err error) {
	if enforce {
		switch blind, err := meterBlind(ctx, c, ns, now); {
		case err != nil:
			return "", err
		case blind:
			return "", ErrMeterBlind
		}
	}
	name := LedgerPrefix + now.UTC().Format(LedgerDayLayout)
	key := types.NamespacedName{Namespace: ns, Name: name}
	for range LedgerAttempts {
		cm := &corev1.ConfigMap{}
		err := c.Get(ctx, key, cm)
		existed := true
		if apierrors.IsNotFound(err) {
			cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
			existed = false
		} else if err != nil {
			return "", fmt.Errorf("ledger %s: %w", name, err)
		}
		spent, err := LedgerSpentColumn(cm)
		if err != nil {
			return "", err
		}
		reserved, err := ledgerReservations(cm)
		if err != nil {
			return "", err
		}
		// R48: the run id is the retry's identity. Our own equal entry means we already
		// landed this admission: pass without reserving twice. A different value under this
		// id is another run's slot, and overwriting it would unreserve theirs: refuse.
		if prev, ok := reserved[id]; ok {
			if prev == mine {
				return "", nil
			}
			return "", ErrRunBusy
		}
		over = ""
		switch {
		case mine.Person != "" && OverDaily(dayUsed(spent, reserved, mine.Person), mine.MaxMicroUSD, personLimit):
			over = mine.Person
			if enforce {
				return over, ErrPersonOverBudget
			}
		case OverDaily(dayUsed(spent, reserved, mine.Principal), mine.MaxMicroUSD, limit):
			over = mine.Principal
			if enforce {
				return over, ErrOverBudget
			}
		}
		if mine.Room != "" {
			for _, v := range reserved {
				if v.Room == mine.Room {
					return over, ErrRoomBusy
				}
			}
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		b, err := json.Marshal(mine)
		if err != nil {
			return "", fmt.Errorf("ledger %s: %w", name, err)
		}
		cm.Data[LedgerReserved+id] = string(b)
		if existed {
			err = c.Update(ctx, cm)
		} else {
			err = c.Create(ctx, cm) // a concurrent Create conflicts; the loop re-reads
		}
		switch {
		case err == nil:
			return over, nil
		case apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err):
			continue // re-read and re-check (R50)
		default:
			return "", fmt.Errorf("ledger %s: %w", name, err)
		}
	}
	// Conflicts at this depth mean steady contention, not a lost slot: fail open never.
	return "", ErrLedgerBusy
}

// dropReservation undoes reserve when the run creation itself failed. Best effort: a leaked
// entry costs one day of budget, while a delete that fights a concurrent replica could drop
// another admission's reservation instead.
func (s *Server) dropReservation(ctx context.Context, id string) {
	key := types.NamespacedName{Namespace: s.Namespace, Name: LedgerPrefix + s.Now().UTC().Format(LedgerDayLayout)}
	cm := &corev1.ConfigMap{}
	if err := s.Rooms.Get(ctx, key, cm); err != nil {
		return
	}
	if _, ok := cm.Data[LedgerReserved+id]; !ok {
		return
	}
	delete(cm.Data, LedgerReserved+id)
	_ = s.Rooms.Update(ctx, cm)
}

// OverDaily is the daily cap's one decision (R50), in micro-USD (TC7): a run that may spend
// maxMicroUSD does not fit beside what the principal's day already holds. The API's admission and
// the factory's Queued (N2) both decide on it.
func OverDaily(usedMicroUSD, maxMicroUSD, limit int64) bool { return usedMicroUSD+maxMicroUSD > limit }

// dayUsed is what principal holds of its day, in micro-USD: the meter's settled spend plus its
// admitted runs' reservations, as their principal or their person.
func dayUsed(spent map[string]int64, reserved map[string]Reservation, principal string) int64 {
	used := spent[principal]
	for _, v := range reserved {
		if v.Principal == principal || v.Person == principal {
			used += v.MaxMicroUSD
		}
	}
	return used
}

// DayUsed reads principal's use of the UTC day of now from that day's ledger, in micro-USD, as
// admission does: a missing ledger is a day with nothing used. c must read ConfigMaps live: the
// factory's cached watch sees the stop object only.
func DayUsed(ctx context.Context, c client.Reader, ns, principal string, now time.Time) (int64, error) {
	cm := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: LedgerPrefix + now.UTC().Format(LedgerDayLayout)}, cm)
	switch {
	case apierrors.IsNotFound(err):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("ledger: %w", err)
	}
	spent, err := LedgerSpentColumn(cm)
	if err != nil {
		return 0, err
	}
	reserved, err := ledgerReservations(cm)
	if err != nil {
		return 0, err
	}
	return dayUsed(spent, reserved, principal), nil
}

// LedgerSpentColumn reads a ledger's spent map, nil when the column is empty. Exported with
// the layout constants above so admission and the meter parse and write one encoding.
func LedgerSpentColumn(cm *corev1.ConfigMap) (map[string]int64, error) {
	raw, ok := cm.Data[LedgerSpent]
	if !ok || raw == "" {
		return nil, nil
	}
	var m map[string]int64
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("ledger %s: %w", cm.Name, err)
	}
	return m, nil
}

// SetLedgerSpentColumn writes the spent map as the ledger's one JSON column.
func SetLedgerSpentColumn(cm *corev1.ConfigMap, spent map[string]int64) error {
	b, err := json.Marshal(spent)
	if err != nil {
		return fmt.Errorf("ledger %s spent: %w", cm.Name, err)
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[LedgerSpent] = string(b)
	return nil
}

func ledgerReservations(cm *corev1.ConfigMap) (map[string]Reservation, error) {
	m := map[string]Reservation{}
	for k, raw := range cm.Data {
		id, ok := strings.CutPrefix(k, LedgerReserved)
		if !ok {
			continue
		}
		var r Reservation
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, fmt.Errorf("ledger %s reservation %s: %w", cm.Name, id, err)
		}
		m[id] = r
	}
	return m, nil
}
