// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	factoryv1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/authn"
	"github.com/Smana/maku/internal/envelope"
	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/finding"
	"github.com/Smana/maku/internal/factory/taskid"
)

var runlore = authn.Principal{Kind: envelope.ActorSystem, ID: "system:runlore", Sponsored: true, Internal: true, AccessToken: "t"}

// key is a TriggerKey, as RunLore sends it.
const key = "kubepodcrashlooping|apps|deployment|web|gcp-0"

// incident is RunLore's body for a finding of confidence conf.
func incident(conf float64) string {
	return fmt.Sprintf(`{"repository":"Smana/cloud-native-ref","text":"the pod restarts on OOM","dataClass":"internal",`+
		`"maxCostUSD":2,"incident":{"alert":"KubePodCrashLooping","resource":"apps/web","severity":"critical",`+
		`"cluster":"gcp-0","verdict":"action_required","confidence":%g}}`, conf)
}

func internalCfg() *config.Config {
	c := sponsoredCfg()
	c.API.InternalPrincipals = []config.InternalPrincipal{{Sponsored: config.Sponsored{Name: "runlore", ClientIDFile: "/r",
		Sponsor: "Smana", Repositories: []string{"Smana/cloud-native-ref"}, DailyUSD: 10, MaxCostUSD: 2, MaxOpenTasks: 3},
		MinConfidence: 0.75, DailyTasks: 5, Verification: true}}
	return c
}

func internalServer(t *testing.T, objs ...client.Object) (*Server, *fakeFiler) {
	t.Helper()
	s, f := newTaskServer(t, runlore, 0, objs...)
	s.Cfg = internalCfg()
	return s, f
}

// RA4–RA6, offline proof 3: an internal principal gets internal and investigate's inputs, a stub issue,
// and today's dedup.
func TestCreateInternalTask(t *testing.T) {
	s, f := internalServer(t)
	w := postTask(s, incident(0.9), key)
	if w.Code != http.StatusCreated {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	got := tasks(t, s)
	if len(got) != 1 {
		t.Fatalf("%d tasks", len(got))
	}
	tk := got[0]
	want := taskid.Name("api:runlore:" + key + ":gen1")
	if tk.Name != want || tk.Spec.DataClass != "internal" || tk.Spec.Source.Kind != "api" ||
		tk.Spec.Source.RequestedBy != "system:runlore" || tk.Spec.Source.Key != "api:runlore:"+key ||
		tk.Labels[finding.LabelSourceKey] != taskid.Name("api:runlore:"+key) || tk.Spec.MaxMicroUSD != 2_000_000 ||
		tk.Spec.Source.Sponsor != "github:smana" || !strings.HasPrefix(tk.Spec.Text, "# ") {
		t.Fatalf("%+v", tk)
	}
	if len(f.got) != 1 || f.got[0].labels[0] != "factory/proposed" || strings.Contains(f.got[0].body, "OOM") {
		t.Fatalf("the stub issue names the alert and the room, never the finding: %+v", f.got)
	}
	if tk.Labels[factoryv1.LabelIssue] != "42" || tk.Spec.Issue != 42 {
		t.Fatalf("the stub is linked and labelled, as the poller finds a task by it: %v %d", tk.Labels, tk.Spec.Issue)
	}
	if w := postTask(s, incident(0.9), key); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"duplicate":true`) {
		t.Fatalf("a replay while open: %d %s", w.Code, w.Body)
	}
}

// R4: once the open task ended, the same key is a new generation.
func TestInternalTaskNewGenerationAfterDone(t *testing.T) {
	ended := &factoryv1.Task{ObjectMeta: metav1.ObjectMeta{Name: taskid.Name("api:runlore:" + key + ":gen1"), Namespace: "agent-system",
		Labels: map[string]string{finding.LabelSourceKey: taskid.Name("api:runlore:" + key)}},
		Spec:   factoryv1.TaskSpec{Source: factoryv1.Source{Kind: "api", RequestedBy: "system:runlore", Key: "api:runlore:" + key}},
		Status: factoryv1.TaskStatus{Phase: factoryv1.PhaseDone}}
	s, _ := internalServer(t, ended)
	if w := postTask(s, incident(0.9), key); w.Code != http.StatusCreated {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if _, found, _ := s.existing(t.Context(), taskid.Name("api:runlore:"+key+":gen2")); !found {
		t.Fatal("no gen2")
	}
}

// RA3, R5, offline proof 3: the gates, and the sponsored rule unchanged.
func TestInternalTaskGates(t *testing.T) {
	for name, c := range map[string]struct {
		p      authn.Principal
		body   string
		seed   int
		status int
		reason string
	}{
		"0.74 is below the threshold":         {runlore, incident(0.74), 0, http.StatusAccepted, "below_threshold"},
		"the sixth task of the day":           {runlore, incident(0.9), 5, http.StatusTooManyRequests, "principal_daily_tasks"},
		"a title":                             {runlore, strings.Replace(incident(0.9), `"text"`, `"title":"x: y","text"`, 1), 0, http.StatusBadRequest, "title_not_allowed"},
		"onBehalfOf":                          {runlore, strings.Replace(incident(0.9), `"text"`, `"onBehalfOf":"slack:T01ABC/U02DEF","text"`, 1), 0, http.StatusBadRequest, "on_behalf_of_not_allowed"},
		"public data":                         {runlore, strings.Replace(incident(0.9), `"internal"`, `"public"`, 1), 0, http.StatusBadRequest, "bad_data_class"},
		"no incident":                         {runlore, `{"repository":"Smana/cloud-native-ref","text":"x","dataClass":"internal","maxCostUSD":2}`, 0, http.StatusBadRequest, "incident_required"},
		"a sponsored principal asks internal": {slack, strings.Replace(draft, `"public"`, `"internal"`, 1), 0, http.StatusBadRequest, "bad_data_class"},
	} {
		t.Run(name, func(t *testing.T) {
			var seeded []client.Object
			for i := range c.seed {
				seeded = append(seeded, &factoryv1.Task{ObjectMeta: metav1.ObjectMeta{Name: taskid.Name(fmt.Sprint("seed", i)),
					Namespace: "agent-system", CreationTimestamp: metav1.NewTime(now)},
					Spec:   factoryv1.TaskSpec{Source: factoryv1.Source{Kind: "runlore", RequestedBy: "system:runlore"}},
					Status: factoryv1.TaskStatus{Phase: factoryv1.PhaseDone}})
			}
			s, f := internalServer(t, seeded...)
			s.Auth = byToken{"t": c.p}
			w := postTask(s, c.body, key)
			if w.Code != c.status || !strings.Contains(w.Body.String(), c.reason) || len(f.got) != 0 {
				t.Fatalf("%d %s, %d issues", w.Code, w.Body, len(f.got))
			}
		})
	}
}

// An Idempotency-Key past 255 bytes is RunLore's to hash; the factory refuses it as today.
func TestInternalTaskKeyBound(t *testing.T) {
	s, _ := internalServer(t)
	if w := postTask(s, incident(0.9), strings.Repeat("k", 256)); w.Code != http.StatusBadRequest {
		t.Fatalf("%d", w.Code)
	}
}

// The principal is an authentication surface: what the plan does not grant it is refused.
func TestInternalPrincipalRefusals(t *testing.T) {
	t.Run("a bare run", func(t *testing.T) {
		st := &store{}
		c := fake.NewClientBuilder().WithScheme(scheme()).Build()
		s := mkServer(t, st, c, false, false, "7f3cq2xz", 5_000_000)
		s.Cfg, s.Auth = internalCfg(), byToken{"runlore": runlore}
		w := post(s.Handler(), "runlore", base())
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "sponsored_only_tasks") || len(st.created) != 0 {
			t.Fatalf("%d %s %d", w.Code, w.Body, len(st.created))
		}
	})
	t.Run("an internal token the config does not declare", func(t *testing.T) {
		s, f := newTaskServer(t, runlore, 0) // sponsoredCfg: no internal principals
		w := postTask(s, incident(0.9), key)
		if w.Code != http.StatusForbidden || len(tasks(t, s)) != 0 || len(f.got) != 0 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	})
	t.Run("a repository the principal does not list", func(t *testing.T) {
		s, f := internalServer(t)
		s.Cfg.API.InternalPrincipals[0].Repositories = []string{"Smana/other"}
		w := postTask(s, incident(0.9), key)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "repository_not_allowed") || len(f.got) != 0 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	})
	t.Run("a ceiling over the principal's", func(t *testing.T) {
		s, f := internalServer(t)
		w := postTask(s, strings.Replace(incident(0.9), `"maxCostUSD":2`, `"maxCostUSD":3`, 1), key)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "max_cost_over_principal") || len(f.got) != 0 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	})
	t.Run("a ref the sponsored path would take", func(t *testing.T) {
		s, _ := internalServer(t)
		w := postTask(s, strings.Replace(incident(0.9), `"text"`, `"ref":"main","text"`, 1), key)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	})
	t.Run("an incident on the sponsored path", func(t *testing.T) {
		s, f := newTaskServer(t, slack, 0)
		body := strings.Replace(draft, `"title"`, `"incident":{"alert":"A","resource":"r"},"title"`, 1)
		w := postTask(s, body, key)
		if w.Code != http.StatusBadRequest || len(f.got) != 0 || len(tasks(t, s)) != 0 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	})
	t.Run("the principal's day is a bound", func(t *testing.T) {
		s, _ := internalServer(t)
		s.Rooms = fake.NewClientBuilder().WithScheme(scheme()).WithObjects(
			ledgerCM(now.UTC().Format(LedgerDayLayout), map[string]int64{"system:runlore": 9_000_000})).Build()
		w := postTask(s, incident(0.9), key)
		if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), reasonPrincipalOver) {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	})
	t.Run("maxOpenTasks", func(t *testing.T) {
		var open []client.Object
		for _, n := range []string{"o1", "o2", "o3"} {
			tk := openTask(n)
			tk.Spec.Source.RequestedBy = "system:runlore"
			open = append(open, tk)
		}
		s, _ := internalServer(t, open...)
		w := postTask(s, incident(0.9), key)
		if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "principal_task_cap") {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	})
}
