// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	factoryv1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/config"
)

const (
	origTask = "orig2222"
	fixTask  = "fixt2222"
)

var otherInternal = authn.Principal{Kind: envelope.ActorSystem, ID: "system:other", Sponsored: true, Internal: true, AccessToken: "t"}

// verifyCfg is internalCfg with a class that waits 2 h for the verdict, and a second internal
// principal that verifies too.
func verifyCfg() *config.Config {
	c := internalCfg()
	c.Repositories[0].Classes = map[string]config.Class{"alert-rules": {Monitor: &config.Monitor{
		FollowUpWindow: config.Duration{Duration: 168 * time.Hour},
		Verification:   &config.VerificationWatch{Within: config.Duration{Duration: 2 * time.Hour}}}}}
	c.API.InternalPrincipals = append(c.API.InternalPrincipals, config.InternalPrincipal{Sponsored: config.Sponsored{Name: "other",
		ClientIDFile: "/o", Sponsor: "Smana", Repositories: []string{"Smana/cloud-native-ref"}, DailyUSD: 10, MaxCostUSD: 2, MaxOpenTasks: 3},
		MinConfidence: 0.75, DailyTasks: 5, Verification: true})
	return c
}

// verifyTasks is the origin task and its fix, merged mergedAgo ago.
func verifyTasks(mergedAgo time.Duration, origin string) []client.Object {
	merged := metav1.NewTime(now.Add(-mergedAgo))
	return []client.Object{
		&factoryv1.Task{ObjectMeta: metav1.ObjectMeta{Name: origTask, Namespace: "agent-system"},
			Spec:   factoryv1.TaskSpec{Repository: "Smana/cloud-native-ref", Source: factoryv1.Source{Kind: "api", RequestedBy: "system:runlore"}},
			Status: factoryv1.TaskStatus{Published: &factoryv1.Published{Issue: 42, By: "smana", At: metav1.NewTime(now.Add(-48 * time.Hour))}}},
		&factoryv1.Task{ObjectMeta: metav1.ObjectMeta{Name: fixTask, Namespace: "agent-system"},
			Spec: factoryv1.TaskSpec{Repository: "Smana/cloud-native-ref", PredictedClass: "alert-rules", Origin: origin,
				Source: factoryv1.Source{Kind: "issue", RequestedBy: "github:smana"}},
			Status: factoryv1.TaskStatus{Phase: factoryv1.PhaseDone,
				PullRequest: &factoryv1.PullRequestRef{Number: 12, MergedAt: &merged}}},
	}
}

// verifyServer is newTaskServer with a Task status subresource, as the CRD declares.
func verifyServer(t *testing.T, p authn.Principal, objs ...client.Object) *Server {
	t.Helper()
	s, _ := newTaskServer(t, p, 0)
	s.Rooms = fake.NewClientBuilder().WithScheme(scheme()).WithStatusSubresource(&factoryv1.Task{}).
		WithObjects(append(objs, ledgerCM(now.UTC().Format(LedgerDayLayout), map[string]int64{}))...).Build()
	s.Cfg = verifyCfg()
	return s
}

func verifyBody(verdict, evidence string) string {
	return `{"verdict":"` + verdict + `","alert":{"name":"KubePodCrashLooping","firingAtMerge":true,"firingNow":false},` +
		`"checkedAt":"` + now.Format(time.RFC3339) + `","evidence":"` + evidence + `"}`
}

func postVerification(s *Server, task, body string, bearer bool) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/tasks/"+task+"/verification", strings.NewReader(body))
	if bearer {
		r.Header.Set("Authorization", "Bearer t")
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func getTask(t *testing.T, s *Server, name string) factoryv1.Task {
	t.Helper()
	var tk factoryv1.Task
	if err := s.Rooms.Get(t.Context(), client.ObjectKey{Namespace: "agent-system", Name: name}, &tk); err != nil {
		t.Fatal(err)
	}
	return tk
}

// RA11, R11, offline proof 4: the endpoint records RunLore's verdict on its own fix, once.
func TestRecordVerification(t *testing.T) {
	for name, c := range map[string]struct {
		p       authn.Principal
		mutate  func(*Server)
		objs    []client.Object
		task    string
		body    string
		noToken bool
		status  int
		reason  string
	}{
		"a verdict on its fix":        {p: runlore, objs: verifyTasks(30*time.Minute, origTask), task: fixTask, body: verifyBody("resolved", "pods stable for 30m"), status: 201, reason: `"verdict":"resolved"`},
		"another internal principal":  {p: otherInternal, objs: verifyTasks(30*time.Minute, origTask), task: fixTask, body: verifyBody("resolved", "x"), status: 403, reason: "not_own_task"},
		"a task with no origin":       {p: runlore, objs: verifyTasks(30*time.Minute, ""), task: fixTask, body: verifyBody("resolved", "x"), status: 403, reason: "not_own_task"},
		"a sponsored principal":       {p: slack, objs: verifyTasks(30*time.Minute, origTask), task: fixTask, body: verifyBody("resolved", "x"), status: 403, reason: "not_permitted"},
		"verification false":          {p: runlore, mutate: func(s *Server) { s.Cfg.API.InternalPrincipals[0].Verification = false }, objs: verifyTasks(30*time.Minute, origTask), task: fixTask, body: verifyBody("resolved", "x"), status: 403, reason: "verification_not_allowed"},
		"not merged":                  {p: runlore, objs: unmerged(), task: fixTask, body: verifyBody("resolved", "x"), status: 409, reason: "not_merged"},
		"merged three hours ago":      {p: runlore, objs: verifyTasks(3*time.Hour, origTask), task: fixTask, body: verifyBody("resolved", "x"), status: 409, reason: "window_closed"},
		"an unknown task":             {p: runlore, objs: verifyTasks(30*time.Minute, origTask), task: "nope2222", body: verifyBody("resolved", "x"), status: 404, reason: "no_such_task"},
		"a verdict out of the enum":   {p: runlore, objs: verifyTasks(30*time.Minute, origTask), task: fixTask, body: verifyBody("fixed", "x"), status: 400, reason: "bad_verdict"},
		"17 KiB of evidence":          {p: runlore, objs: verifyTasks(30*time.Minute, origTask), task: fixTask, body: verifyBody("resolved", strings.Repeat("a", 17<<10)), status: 400, reason: "bad_evidence"},
		"evidence the redactor flags": {p: runlore, objs: verifyTasks(30*time.Minute, origTask), task: fixTask, body: verifyBody("resolved", "ghp_"+strings.Repeat("aB3dE5gH7j", 3)+"aB3dE5"), status: 400, reason: "bad_evidence"},
		"an unknown field":            {p: runlore, objs: verifyTasks(30*time.Minute, origTask), task: fixTask, body: strings.Replace(verifyBody("resolved", "x"), `"verdict"`, `"extra":1,"verdict"`, 1), status: 400, reason: "bad_json"},
		"no token":                    {p: runlore, objs: verifyTasks(30*time.Minute, origTask), task: fixTask, body: verifyBody("resolved", "x"), noToken: true, status: 401, reason: "unauthenticated"},
	} {
		t.Run(name, func(t *testing.T) {
			s := verifyServer(t, c.p, c.objs...)
			if c.mutate != nil {
				c.mutate(s)
			}
			w := postVerification(s, c.task, c.body, !c.noToken)
			if w.Code != c.status || !strings.Contains(w.Body.String(), c.reason) {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if c.status != 201 {
				if fx := getTask(t, s, fixTask); fx.Status.Monitor != nil && fx.Status.Monitor.Verification != nil {
					t.Fatalf("a refusal records nothing: %+v", fx.Status.Monitor)
				}
			}
		})
	}
}

func unmerged() []client.Object {
	o := verifyTasks(0, origTask)
	o[1].(*factoryv1.Task).Status.PullRequest.MergedAt = nil
	return o
}

// A recorded verdict lands on the fix, its evidence on the origin task; a replay is a no-op and
// a different verdict is refused.
func TestRecordVerificationRecordsAndReplays(t *testing.T) {
	s := verifyServer(t, runlore, verifyTasks(30*time.Minute, origTask)...)
	if w := postVerification(s, fixTask, verifyBody("resolved", "pods stable for 30m"), true); w.Code != http.StatusCreated {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	fx, orig := getTask(t, s, fixTask), getTask(t, s, origTask)
	v := fx.Status.Monitor.Verification
	if v == nil || v.Verdict != "resolved" || v.By != "system:runlore" || v.Alert.Name != "KubePodCrashLooping" || !v.Alert.FiringAtMerge || v.Alert.FiringNow ||
		!v.RecordedAt.Time.Equal(now) || !fx.Status.Monitor.Since.Time.Equal(now.Add(-30*time.Minute)) {
		t.Fatalf("%+v", fx.Status.Monitor)
	}
	if p := orig.Status.Published; p == nil || p.Issue != 42 || p.Evidence != "pods stable for 30m" || p.EvidenceFor != fixTask {
		t.Fatalf("the evidence goes to the origin task: %+v", orig.Status.Published)
	}
	if w := postVerification(s, fixTask, verifyBody("resolved", "pods stable for 30m"), true); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"duplicate":true`) {
		t.Fatalf("a replay: %d %s", w.Code, w.Body)
	}
	if w := postVerification(s, fixTask, verifyBody("persisting", "again"), true); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "verdict_already_recorded") {
		t.Fatalf("persisting after resolved: %d %s", w.Code, w.Body)
	}
}

// A replay after the window is a 200 duplicate, not a refusal; and the window counts from the
// monitor's Since when a pass has set it.
func TestVerificationReplayAfterWindowAndSharedSince(t *testing.T) {
	s := verifyServer(t, runlore, verifyTasks(30*time.Minute, origTask)...)
	if w := postVerification(s, fixTask, verifyBody("resolved", "e"), true); w.Code != http.StatusCreated {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	s.Now = func() time.Time { return now.Add(5 * time.Hour) }
	if w := postVerification(s, fixTask, verifyBody("resolved", "e"), true); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "duplicate") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	// The monitor's Since (GitHub's merge time) is earlier than the Task's MergedAt: it decides.
	objs := verifyTasks(10*time.Minute, origTask)
	objs[1].(*factoryv1.Task).Status.Monitor = &factoryv1.MonitorStatus{Since: metav1.NewTime(now.Add(-3 * time.Hour))}
	s = verifyServer(t, runlore, objs...)
	if w := postVerification(s, fixTask, verifyBody("resolved", "e"), true); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "window_closed") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

// A losing racer's evidence never stays: a verdict refused leaves the origin task untouched.
func TestRefusedVerdictLeavesNoEvidence(t *testing.T) {
	s := verifyServer(t, runlore, verifyTasks(30*time.Minute, origTask)...)
	postVerification(s, fixTask, verifyBody("resolved", "first"), true)
	if w := postVerification(s, fixTask, verifyBody("persisting", "loser"), true); w.Code != http.StatusConflict {
		t.Fatalf("%d", w.Code)
	}
	if e := getTask(t, s, origTask).Status.Published.Evidence; e != "first" {
		t.Fatalf("evidence %q", e)
	}
}
