// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/authn"
	"github.com/Smana/maku/internal/envelope"
	"github.com/Smana/maku/internal/factory/api"
	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/forge"
	"github.com/Smana/maku/internal/factory/forge/forgetest"
)

type runloreAuth struct{}

func (runloreAuth) Authenticate(*http.Request) (authn.Principal, error) {
	return authn.Principal{Kind: envelope.ActorSystem, ID: "system:runlore", Sponsored: true, Internal: true, AccessToken: "t"}, nil
}

// RA9, offline proof 3, end to end: POST /v1/tasks files the stub and links it to the task, the task
// ends Done, and a maintainer's factory/publish on that stub finds the task and files the proposal.
// Nothing in the fixture sets the issue label by hand: the intake must.
func TestPublishFollowsAnInternalIntake(t *testing.T) {
	f := forgetest.NewFake()
	p, c := poller(t, f)
	p.Cfg.Maintainers = []string{"Smana"}
	p.Now = func() time.Time { return t0.Add(time.Hour) }
	p.Redact = testRedactor(t)
	p.Proposal = func(context.Context, *v1alpha1.Task) (string, error) { return proposalOK, nil }

	srv := &api.Server{Auth: runloreAuth{}, Rooms: c, Namespace: "agent-system", Now: func() time.Time { return t0 },
		Stopped: func(context.Context) bool { return false }, Owned: func(string) bool { return true },
		Forges: func(string) api.IssueFiler { return f },
		Cfg: &config.Config{RoomsURL: "https://rooms.priv.example",
			Repositories: []config.Repository{{Name: "Smana/cloud-native-ref", Maintainers: []string{"Smana"}}},
			Tiers:        map[string]config.Tier{"standard": {Model: "agent-default", RunUSD: 2.55, TaskUSD: 5.10}},
			Meter:        config.Meter{Prices: map[string]config.Price{"agent-default": {Input: 1.40, CacheRead: 0.26, Output: 4.40}}},
			Budgets:      config.Budgets{FactoryDailyUSD: 25, HumanDailyUSD: 5},
			API: config.API{InternalPrincipals: []config.InternalPrincipal{{Sponsored: config.Sponsored{Name: "runlore", ClientIDFile: "/r",
				Sponsor: "Smana", Repositories: []string{"Smana/cloud-native-ref"}, DailyUSD: 10, MaxCostUSD: 2}, MinConfidence: 0.75, DailyTasks: 5}}}}}
	body := `{"repository":"Smana/cloud-native-ref","text":"OOM","dataClass":"internal","maxCostUSD":2,` +
		`"incident":{"alert":"A","resource":"apps/web","severity":"critical","cluster":"gcp-0","verdict":"action_required","confidence":0.9}}`
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/tasks", strings.NewReader(body))
	r.Header.Set("Idempotency-Key", "k")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var l v1alpha1.TaskList
	if err := c.List(t.Context(), &l); err != nil || len(l.Items) != 1 {
		t.Fatalf("%v %d", err, len(l.Items))
	}
	tk := l.Items[0]
	stub := tk.Spec.Issue
	if stub == 0 {
		t.Fatal("the stub issue is not linked")
	}
	// What triage and the reconciler do: the template, then the end.
	tk.Spec.Template = "investigate"
	if err := c.Update(t.Context(), &tk); err != nil {
		t.Fatal(err)
	}
	tk.Status.Phase = v1alpha1.PhaseDone
	if err := c.Status().Update(t.Context(), &tk); err != nil {
		t.Fatal(err)
	}

	f.SetEvents(stub, forge.LabelEvent{Actor: "Smana", Label: LabelPublish, At: t0})
	f.SetLabeled(LabelPublish, forge.Item{Number: stub})
	if err := p.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := getTask(t, c, tk.Name); got.Status.Published == nil || len(f.Created()) != 2 {
		t.Fatalf("published %+v, %d issues created: %q", got.Status.Published, len(f.Created()), strings.Join(f.Comments(stub), "|"))
	}
}
