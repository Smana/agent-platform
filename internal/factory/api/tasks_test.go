// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	factoryv1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/taskid"
	"github.com/Smana/agent-platform/internal/redact"
)

// F4, invariant 1: a sponsored token never starts a bare run, and reserves nothing on the way.
func TestRunsRefusesSponsored(t *testing.T) {
	st := &store{}
	c := fake.NewClientBuilder().WithScheme(scheme()).Build()
	s := mkServer(t, st, c, false, false, "7f3cq2xz", 5_000_000)
	s.Auth = byToken{"slack": slack}
	w := post(s.Handler(), "slack", base())
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"error":"sponsored_only_tasks"`) || len(st.created) != 0 {
		t.Fatalf("%d %s %d", w.Code, w.Body, len(st.created))
	}
	var cms corev1.ConfigMapList
	if err := c.List(t.Context(), &cms); err != nil || len(cms.Items) != 0 {
		t.Fatalf("a ledger was written: %d %v", len(cms.Items), err)
	}
}

type filed struct {
	title, body string
	labels      []string
}

// fakeFiler files as forgetest.Fake does after the splitter: a forge.Issue, numbered from 42.
type fakeFiler struct{ got []filed }

func (f *fakeFiler) CreateIssue(_ context.Context, title, body string, labels []string) (forge.Issue, error) {
	f.got = append(f.got, filed{title, body, labels})
	return forge.Issue{Number: 41 + len(f.got)}, nil
}

// failingFiler is GitHub down.
type failingFiler struct{}

func (failingFiler) CreateIssue(context.Context, string, string, []string) (forge.Issue, error) {
	return forge.Issue{}, errors.New("github down")
}

var slack = authn.Principal{Kind: envelope.ActorSystem, ID: "system:slack-app", Sponsored: true, AccessToken: "t"}

// draft is a titled request. Its \u200b is JSON's escape inside a raw string: the source holds no
// hidden character, and the decoded text does.
const draft = `{"repository":"Smana/cloud-native-ref","title":"docs(agents): the link is dead",` +
	`"text":"## What is wrong\nx\u200b\n## What it should be\ny\n## Acceptance check\nz","dataClass":"public",` +
	`"maxCostUSD":2,"onBehalfOf":"slack:T01ABC/U02DEF","ref":"slack:T01ABC/C03/1729.0001"}`

// sponsoredCfg is one principal on good's repository and maintainer, with the host's priced tiers.
func sponsoredCfg() *config.Config {
	return &config.Config{RoomsURL: "https://rooms.priv.example",
		Repositories: []config.Repository{{Name: "Smana/cloud-native-ref", Maintainers: []string{"Smana"}}},
		Tiers: map[string]config.Tier{"standard": {Model: "agent-default", RunUSD: 2.55, TaskUSD: 5.10},
			"frontier": {Model: "agent-default", RunUSD: 6.80, TaskUSD: 13.60}},
		Meter:   config.Meter{Prices: map[string]config.Price{"agent-default": {Input: 1.40, CacheRead: 0.26, Output: 4.40}}},
		Budgets: config.Budgets{FactoryDailyUSD: 25, HumanDailyUSD: 5},
		API: config.API{Repositories: []string{"Smana/cloud-native-ref"}, Sponsored: []config.Sponsored{{Name: "slack-app",
			ClientIDFile: "/x", Sponsor: "Smana", Repositories: []string{"Smana/cloud-native-ref"}, DailyUSD: 10, MaxCostUSD: 5,
			OnBehalfOf: config.OnBehalfOf{Slack: map[string]string{"T01ABC/U02DEF": "smana"}}}}}}
}

// newTaskServer is the intake as p's bearer "t" sees it, the principal's day seeded with spent
// micro-USD, and its issues filed by one fakeFiler.
func newTaskServer(t *testing.T, p authn.Principal, spent int64, objs ...client.Object) (*Server, *fakeFiler) {
	t.Helper()
	objs = append(objs, ledgerCM(now.UTC().Format(LedgerDayLayout), map[string]int64{"system:slack-app": spent}))
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(objs...).Build()
	s := mkServer(t, &store{}, c, false, false, "7f3cq2xz", 5_000_000)
	s.Auth, s.Cfg, s.Redact = byToken{"t": p}, sponsoredCfg(), testRedactor(t)
	f := &fakeFiler{}
	s.Forges = func(string) IssueFiler { return f }
	return s, f
}

// postTask posts body to /v1/tasks with bearer "t" and key, when set.
func postTask(s *Server, body, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/tasks", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer t")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func tasks(t *testing.T, s *Server) []factoryv1.Task {
	t.Helper()
	var l factoryv1.TaskList
	if err := s.Rooms.List(t.Context(), &l, client.InNamespace(s.Namespace)); err != nil {
		t.Fatal(err)
	}
	return l.Items
}

// openTask is one of the principal's api tasks, still running.
func openTask(name string) *factoryv1.Task {
	return &factoryv1.Task{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agent-system"},
		Spec:   factoryv1.TaskSpec{Source: factoryv1.Source{Kind: "api", RequestedBy: "system:slack-app"}},
		Status: factoryv1.TaskStatus{Phase: factoryv1.PhaseImplementing}}
}

// F4, R4–R12: admission in its order, both identities on the Task, the issue filed once, no label.
func TestCreateTaskAdmission(t *testing.T) {
	for name, c := range map[string]struct {
		p      authn.Principal
		body   string
		key    string
		edit   func(*config.Config)
		status int
		reason string
	}{
		"a human":            {authn.Principal{Kind: envelope.ActorHuman, ID: "human:1"}, draft, "k", nil, 403, "sponsored_only"},
		"no idempotency key": {slack, draft, "", nil, 400, "idempotency_key_required"},
		"an unknown field":   {slack, strings.Replace(draft, `"ref"`, `"tier":"frontier","ref"`, 1), "k", nil, 400, "bad_json"},
		"another repository": {slack, strings.Replace(draft, "cloud-native-ref", "other", 1), "k", nil, 403, "repository_not_allowed"},
		"unpriced":           {slack, draft, "k", func(c *config.Config) { c.Meter.Prices = nil }, 503, "meter_unpriced"},
		"no max_cost":        {slack, strings.Replace(draft, `"maxCostUSD":2,`, ``, 1), "k", nil, 400, "max_cost_required"},
		"over the principal": {slack, strings.Replace(draft, `"maxCostUSD":2`, `"maxCostUSD":6`, 1), "k", nil, 400, "max_cost_over_principal"},
		"over every class": {slack, strings.Replace(draft, `"maxCostUSD":2`, `"maxCostUSD":14`, 1), "k",
			func(c *config.Config) { c.API.Sponsored[0].MaxCostUSD, c.API.Sponsored[0].DailyUSD = 20, 20 }, 400, "max_cost_over_class"},
		"sponsor_not_linked": {slack, strings.Replace(draft, "U02DEF", "U09XYZ", 1), "k", nil, 403, "not_linked"},
		// R3: a maintainer removed since load is refused at admission too.
		"sponsor_demoted":         {slack, draft, "k", func(c *config.Config) { c.Repositories[0].Maintainers = []string{"someone"} }, 403, "not_a_maintainer"},
		"a bad draft":             {slack, strings.Replace(draft, `\n## Acceptance check\nz`, ``, 1), "k", nil, 400, "invalid_draft"},
		"internal from a machine": {slack, strings.Replace(draft, `"public"`, `"internal"`, 1), "k", nil, 400, "bad_data_class"},
	} {
		t.Run(name, func(t *testing.T) {
			s, f := newTaskServer(t, c.p, 0)
			if c.edit != nil {
				c.edit(s.Cfg)
			}
			rec := postTask(s, c.body, c.key)
			if rec.Code != c.status || !strings.Contains(rec.Body.String(), c.reason) {
				t.Fatalf("%d %s, want %d %s", rec.Code, rec.Body, c.status, c.reason)
			}
			if len(f.got) != 0 || len(tasks(t, s)) != 0 {
				t.Fatal("a refusal filed or created something")
			}
		})
	}
	t.Run("the day is spent", func(t *testing.T) {
		s, f := newTaskServer(t, slack, 9_000_000) // 9 of the principal's 10 USD spent today
		if rec := postTask(s, draft, "k"); rec.Code != 429 || !strings.Contains(rec.Body.String(), "principal_budget_exhausted") {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if len(f.got) != 0 {
			t.Fatal("filed")
		}
	})
	t.Run("principal_task_cap", func(t *testing.T) { // C21: three open tasks, the default cap
		s, f := newTaskServer(t, slack, 0, openTask("aaaaaaa2"), openTask("aaaaaaa3"), openTask("aaaaaaa4"))
		if rec := postTask(s, draft, "k"); rec.Code != 429 || !strings.Contains(rec.Body.String(), "principal_task_cap") {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if len(f.got) != 0 || len(tasks(t, s)) != 3 {
			t.Fatal("a capped request filed or created something")
		}
	})
	t.Run("admitted, then replayed", func(t *testing.T) {
		s, f := newTaskServer(t, slack, 0)
		rec := postTask(s, draft, "k")
		if rec.Code != 201 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		var out struct {
			Task, Room string
			Issue      int
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		ts := tasks(t, s)
		if len(ts) != 1 || out.Room != out.Task || out.Issue != 42 {
			t.Fatalf("%+v %d", out, len(ts))
		}
		got := ts[0].Spec
		if got.Source.Kind != "api" || got.Source.RequestedBy != "system:slack-app" || got.Source.Sponsor != "github:smana" ||
			got.Source.OnBehalfOf != "slack:T01ABC/U02DEF" || got.Source.Trust != "untrusted" || got.MaxMicroUSD != 2_000_000 ||
			got.Issue != 42 || got.IssueAuthor != "smana" || strings.Contains(got.Text, "\u200b") {
			t.Fatalf("%+v", got)
		}
		if len(f.got) != 1 || f.got[0].labels != nil || !strings.Contains(f.got[0].body, "sponsored by @smana through system:slack-app") ||
			strings.Contains(f.got[0].body, "\u200b") {
			t.Fatalf("%+v", f.got)
		}
		if rec := postTask(s, draft, "k"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"duplicate":true`) || len(f.got) != 1 {
			t.Fatalf("the replay: %d %s, %d issues", rec.Code, rec.Body, len(f.got))
		}
	})
	t.Run("no title, no issue", func(t *testing.T) {
		s, f := newTaskServer(t, slack, 0)
		body := strings.Replace(strings.Replace(draft, `"title":"docs(agents): the link is dead",`, "", 1), `"onBehalfOf":"slack:T01ABC/U02DEF",`, "", 1)
		if rec := postTask(s, body, "k2"); rec.Code != 201 || len(f.got) != 0 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if got := tasks(t, s)[0].Spec; got.Issue != 0 || got.Source.Sponsor != "github:smana" || got.Source.OnBehalfOf != "" {
			t.Fatalf("the config's sponsor: %+v", got.Source)
		}
	})
}

// R12: a failed filing leaves the Task without an issue; a replay finds it and files nothing.
func TestCreateTaskIssueFails(t *testing.T) {
	s, f := newTaskServer(t, slack, 0)
	s.Forges = func(string) IssueFiler { return failingFiler{} }
	if rec := postTask(s, draft, "k"); rec.Code != 503 || !strings.Contains(rec.Body.String(), "intake_failed") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	s.Forges = func(string) IssueFiler { return f }
	if rec := postTask(s, draft, "k"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"duplicate":true`) || len(f.got) != 0 {
		t.Fatalf("the replay: %d %s, %d issues", rec.Code, rec.Body, len(f.got))
	}
}

// C24: no GitHub connection for the repository refuses before any Task exists.
func TestCreateTaskWithoutAConnection(t *testing.T) {
	s, _ := newTaskServer(t, slack, 0)
	s.Forges = func(string) IssueFiler { return nil }
	if rec := postTask(s, draft, "k"); rec.Code != 503 || len(tasks(t, s)) != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// R30: with no principal declared, the route does not exist.
func TestTasksRouteOffByDefault(t *testing.T) {
	s := &Server{Cfg: &config.Config{}}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/tasks", strings.NewReader(draft)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("%d", rec.Code)
	}
}

var (
	redactorOnce sync.Once
	redactorVal  *redact.Redactor
	redactorErr  error
)

func testRedactor(t *testing.T) *redact.Redactor {
	t.Helper()
	redactorOnce.Do(func() { redactorVal, redactorErr = redact.New() })
	if redactorErr != nil {
		t.Fatal(redactorErr)
	}
	return redactorVal
}

// refusals added by the fix round: each one's code, and nothing filed or created.
func TestCreateTaskRefusals(t *testing.T) {
	secret := "ghp_" + strings.Repeat("aB3dE5gH7j", 3) + "aB3dE5"
	for name, c := range map[string]struct {
		p      authn.Principal
		body   string
		edit   func(*Server)
		status int
		reason string
	}{
		"kill switch":            {slack, draft, func(s *Server) { s.Stopped = func(context.Context) bool { return true } }, 503, "kill_switch"},
		"unauthenticated":        {authn.Principal{}, draft, func(s *Server) { s.Auth = byToken{} }, 401, "unauthenticated"},
		"not owned":              {slack, draft, func(s *Server) { s.Owned = func(string) bool { return false } }, 409, "repository_not_owned"},
		"bad ref":                {slack, strings.Replace(draft, `"ref":"slack`, `"ref":"`+strings.Repeat("x", 300)+`slack`, 1), nil, 400, "bad_ref"},
		"bad text":               {slack, `{"repository":"Smana/cloud-native-ref","text":"  ","dataClass":"public","maxCostUSD":2}`, nil, 400, "bad_text"},
		"unprefixed link":        {slack, strings.Replace(draft, "slack:T01ABC", "T01ABC", 1), nil, 403, "not_linked"},
		"huge ceiling":           {slack, strings.Replace(draft, `"maxCostUSD":2`, `"maxCostUSD":1e300`, 1), nil, 400, "max_cost_over_principal"},
		"a ceiling under a cent": {slack, strings.Replace(draft, `"maxCostUSD":2`, `"maxCostUSD":0.009`, 1), nil, 400, "max_cost_required"},
		"negative ceiling":       {slack, strings.Replace(draft, `"maxCostUSD":2`, `"maxCostUSD":-3`, 1), nil, 400, "max_cost_required"},
		"a secret in issue":      {slack, strings.Replace(draft, `x\u200b`, secret, 1), nil, 400, "invalid_draft"},
	} {
		t.Run(name, func(t *testing.T) {
			s, f := newTaskServer(t, c.p, 0)
			if c.edit != nil {
				c.edit(s)
			}
			rec := postTask(s, c.body, "k")
			if rec.Code != c.status || !strings.Contains(rec.Body.String(), c.reason) {
				t.Fatalf("%d %s, want %d %s", rec.Code, rec.Body, c.status, c.reason)
			}
			if len(f.got) != 0 || len(tasks(t, s)) != 0 {
				t.Fatal("a refusal filed or created something")
			}
		})
	}
}

// A replay under another principal sees nothing of the first's task, and a task of the key's name
// that is not this principal's is a conflict, never a replay.
func TestCreateTaskReplayScope(t *testing.T) {
	s, f := newTaskServer(t, slack, 0)
	if rec := postTask(s, draft, "k"); rec.Code != 201 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// another principal, same key: its own name, so its own task and its own issue.
	s.Cfg.API.Sponsored = append(s.Cfg.API.Sponsored, config.Sponsored{Name: "other", Sponsor: "Smana",
		Repositories: []string{"Smana/cloud-native-ref"}, DailyUSD: 10, MaxCostUSD: 5})
	s.Auth = byToken{"t": authn.Principal{Kind: envelope.ActorSystem, ID: "system:other", Sponsored: true}}
	rec := postTask(s, strings.Replace(draft, `"onBehalfOf":"slack:T01ABC/U02DEF",`, "", 1), "k")
	if rec.Code != 201 || strings.Contains(rec.Body.String(), "duplicate") || len(tasks(t, s)) != 2 || len(f.got) != 2 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// a Task squatting on the name without the key's identity.
	s.Auth = byToken{"t": slack}
	ts := tasks(t, s)
	for i := range ts {
		if ts[i].Spec.Source.RequestedBy == "system:slack-app" {
			ts[i].Spec.Source.Key = "api:slack-app:elsewhere"
			if err := s.Rooms.Update(t.Context(), &ts[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if rec := postTask(s, draft, "k"); rec.Code != 409 || !strings.Contains(rec.Body.String(), "idempotency_conflict") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// The first Patch conflicts: the retry links the issue, so the Task is not orphaned.
func TestCreateTaskPatchRetries(t *testing.T) {
	fails := 1
	c := fake.NewClientBuilder().WithScheme(scheme()).
		WithObjects(ledgerCM(now.UTC().Format(LedgerDayLayout), nil)).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, p client.Patch, o ...client.PatchOption) error {
			if fails > 0 {
				fails--
				return apierrors.NewConflict(schema.GroupResource{Group: "factory", Resource: "tasks"}, obj.GetName(), errors.New("stale"))
			}
			return cl.Patch(ctx, obj, p, o...)
		}}).Build()
	s := mkServer(t, &store{}, c, false, false, "7f3cq2xz", 5_000_000)
	f := &fakeFiler{}
	s.Auth, s.Cfg, s.Redact = byToken{"t": slack}, sponsoredCfg(), testRedactor(t)
	s.Forges = func(string) IssueFiler { return f }
	if rec := postTask(s, draft, "k"); rec.Code != 201 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if ts := tasks(t, s); len(ts) != 1 || ts[0].Spec.Issue != 42 || fails != 0 {
		t.Fatalf("%+v", ts)
	}
}

// An untitled request's text is scanned too: nothing is created for a secret.
func TestCreateTaskUntitledSecret(t *testing.T) {
	s, f := newTaskServer(t, slack, 0)
	secret := "ghp_" + strings.Repeat("aB3dE5gH7j", 3) + "aB3dE5"
	body := `{"repository":"Smana/cloud-native-ref","text":"use ` + secret + ` here","dataClass":"public","maxCostUSD":2}`
	if rec := postTask(s, body, "k"); rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid_draft") ||
		len(f.got) != 0 || len(tasks(t, s)) != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// No redactor fails closed, creating nothing.
func TestCreateTaskWithoutARedactor(t *testing.T) {
	s, f := newTaskServer(t, slack, 0)
	s.Redact = nil
	if rec := postTask(s, draft, "k"); rec.Code != 503 || !strings.Contains(rec.Body.String(), "intake_failed") ||
		len(f.got) != 0 || len(tasks(t, s)) != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// The AlreadyExists race: the early read finds nothing, Create loses to a Task of the name that is
// not this principal's for this key, and the answer is a conflict, not a duplicate.
func TestCreateTaskRaceConflict(t *testing.T) {
	squat := openTask(taskid.Name("api:slack-app:k"))
	squat.Spec.Source.Key = "api:slack-app:elsewhere"
	first := true
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(squat, ledgerCM(now.UTC().Format(LedgerDayLayout), nil)).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, cl client.WithWatch, k client.ObjectKey, o client.Object, opts ...client.GetOption) error {
			if _, ok := o.(*factoryv1.Task); ok && first {
				first = false
				return apierrors.NewNotFound(schema.GroupResource{Group: "factory", Resource: "tasks"}, k.Name)
			}
			return cl.Get(ctx, k, o, opts...)
		}}).Build()
	s := mkServer(t, &store{}, c, false, false, "7f3cq2xz", 5_000_000)
	f := &fakeFiler{}
	s.Auth, s.Cfg, s.Redact = byToken{"t": slack}, sponsoredCfg(), testRedactor(t)
	s.Forges = func(string) IssueFiler { return f }
	if rec := postTask(s, draft, "k"); rec.Code != 409 || !strings.Contains(rec.Body.String(), "idempotency_conflict") || len(f.got) != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// A link that never lands names the filed issue in the log, so an operator can link it by hand.
func TestCreateTaskOrphanIsLogged(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(ledgerCM(now.UTC().Format(LedgerDayLayout), nil)).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return errors.New("apiserver down")
		}}).Build()
	s := mkServer(t, &store{}, c, false, false, "7f3cq2xz", 5_000_000)
	var logs strings.Builder
	s.Log = slog.New(slog.NewTextHandler(&logs, nil))
	s.Auth, s.Cfg, s.Redact = byToken{"t": slack}, sponsoredCfg(), testRedactor(t)
	s.Forges = func(string) IssueFiler { return &fakeFiler{} }
	if rec := postTask(s, draft, "k"); rec.Code != 503 || !strings.Contains(logs.String(), "issue=42") {
		t.Fatalf("%d %s\n%s", rec.Code, rec.Body, logs.String())
	}
}
