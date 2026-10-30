// SPDX-License-Identifier: Apache-2.0

package runctl

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/Smana/maku/api/v1alpha1"
	"github.com/Smana/maku/internal/factory/runs"
	"github.com/Smana/maku/internal/runwatch"
)

// podBlind reads every pod as NotFound, like an informer cache that is behind.
type podBlind struct{ client.Client }

func (b podBlind) Get(ctx context.Context, k client.ObjectKey, o client.Object, opts ...client.GetOption) error {
	if _, ok := o.(*corev1.Pod); ok {
		return apierrors.NewNotFound(corev1.Resource("pods"), k.Name)
	}
	return b.Client.Get(ctx, k, o, opts...)
}

// cnpFails refuses every CiliumNetworkPolicy create with err: envtest has no RuntimeClass
// admission, so the API server's refusal is injected.
type cnpFails struct {
	client.Client
	err error
}

func (c cnpFails) Create(ctx context.Context, o client.Object, opts ...client.CreateOption) error {
	if o.GetObjectKind().GroupVersionKind().Kind == "CiliumNetworkPolicy" {
		return c.err
	}
	return c.Client.Create(ctx, o, opts...)
}

type env struct {
	c   client.Client
	r   *Reconciler
	ctx context.Context
}

func startEnv(t *testing.T) env {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset: run `task test`, which sets it")
	}
	ctrl.SetLogger(logr.Discard())
	e := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd"), "testdata"}, ErrorIfCRDPathMissing: true}
	cfg, err := e.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Stop() })
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for _, o := range []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "agents"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "agent-platform"}},
		&nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "gvisor"}, Handler: "runsc"},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "agent-git", Namespace: "agents"}, Data: map[string][]byte{"token": []byte("github_pat_x")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "room-broker-tls", Namespace: "agent-platform"}, Data: map[string][]byte{"ca.crt": []byte("CA")}},
	} {
		if err := c.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	return env{c: c, ctx: ctx, r: &Reconciler{Client: c, APIReader: c, Cfg: testConfig(t), Now: func() time.Time { return now }, Log: slog.New(slog.DiscardHandler)}}
}

func (e env) modelSecret(t *testing.T) {
	t.Helper()
	if err := e.c.Create(e.ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "agent-model", Namespace: "agents"},
		Data: map[string][]byte{"api_key": []byte("sk"), "model": []byte("claude-sonnet-5-5")}}); err != nil {
		t.Fatal(err)
	}
}

func (e env) reconcile(t *testing.T, name string) ctrl.Result {
	t.Helper()
	res, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key(name)})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (e env) get(t *testing.T, name string) *v1alpha1.AgentRun {
	t.Helper()
	r := &v1alpha1.AgentRun{}
	if err := e.c.Get(e.ctx, key(name), r); err != nil {
		t.Fatal(err)
	}
	return r
}

func key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: "agents", Name: name}
}

// markReady is what the kubelet reports once the pod runs.
func (e env) markReady(t *testing.T, name string) {
	t.Helper()
	e.setPod(t, name, func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodRunning
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	})
}

func (e env) setPod(t *testing.T, name string, mut func(*corev1.Pod)) {
	t.Helper()
	p := &corev1.Pod{}
	if err := e.c.Get(e.ctx, key(name), p); err != nil {
		t.Fatal(err)
	}
	mut(p)
	if err := e.c.Status().Update(e.ctx, p); err != nil {
		t.Fatal(err)
	}
}

func create(t *testing.T, e env, r *v1alpha1.AgentRun) {
	t.Helper()
	r.UID, r.ResourceVersion = "", ""
	if err := e.c.Create(e.ctx, r); err != nil {
		t.Fatal(err)
	}
}

func TestEnvtestRunLifecycle(t *testing.T) {
	e := startEnv(t)
	e.modelSecret(t)
	create(t, e, run("run-abcdefgh"))
	e.reconcile(t, "run-abcdefgh")

	got := e.get(t, "run-abcdefgh")
	if got.Status.Phase != "Pending" || got.Status.RunID != "abcdefgh" || got.Status.Branch != "agent/abcdefgh" || got.Status.ConversationID == "" {
		t.Fatalf("status after create %+v", got.Status)
	}
	for _, o := range []client.Object{&corev1.ServiceAccount{}, &corev1.Pod{}} {
		if err := e.c.Get(e.ctx, key("run-abcdefgh"), o); err != nil {
			t.Fatalf("%T: %v", o, err)
		}
		if ref := metav1.GetControllerOf(o); ref == nil || ref.Kind != "AgentRun" {
			t.Fatalf("%T is not owned by the run", o)
		}
	}
	for _, n := range []string{"run-abcdefgh-task", "run-abcdefgh-ca"} {
		if err := e.c.Get(e.ctx, key(n), &corev1.ConfigMap{}); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
	}
	cnp := &unstructured.Unstructured{}
	cnp.SetAPIVersion("cilium.io/v2")
	cnp.SetKind("CiliumNetworkPolicy")
	if err := e.c.Get(e.ctx, key("run-abcdefgh"), cnp); err != nil {
		t.Fatalf("cnp: %v", err)
	}

	e.markReady(t, "run-abcdefgh")
	e.reconcile(t, "run-abcdefgh")
	if got := e.get(t, "run-abcdefgh"); got.Status.Phase != "Running" || got.Status.StartedAt == "" {
		t.Fatalf("running %+v", got.Status)
	}

	e.setPod(t, "run-abcdefgh", func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodSucceeded
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "harness", Image: "x", ImageID: "x", State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, StartedAt: metav1.NewTime(t0), FinishedAt: metav1.NewTime(t0.Add(5 * time.Minute))}}}}
	})
	e.reconcile(t, "run-abcdefgh")
	got = e.get(t, "run-abcdefgh")
	if got.Status.Phase != "Succeeded" || got.Status.FinishedAt == "" {
		t.Fatalf("succeeded %+v", got.Status)
	}
	if run, ok := runwatch.FromUnstructured(toUnstructured(t, got)); ok {
		t.Fatalf("runwatch must not read a native run under the default kind: %+v", run)
	}
}

func TestEnvtestRevokeDeletesThePod(t *testing.T) {
	e := startEnv(t)
	e.modelSecret(t)
	create(t, e, run("run-bbbbbbbb"))
	e.reconcile(t, "run-bbbbbbbb")
	r := e.get(t, "run-bbbbbbbb")
	r.Annotations = map[string]string{runwatch.RevokedAnnotation: "manual"}
	if err := e.c.Update(e.ctx, r); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t, "run-bbbbbbbb")
	if got := e.get(t, "run-bbbbbbbb"); got.Status.Phase != "Revoked" || got.Status.Reason != "manual" || got.Status.FinishedAt == "" {
		t.Fatalf("revoked %+v", got.Status)
	}
	p := &corev1.Pod{}
	err := e.c.Get(e.ctx, key("run-bbbbbbbb"), p)
	if err == nil && p.DeletionTimestamp == nil {
		t.Fatal("the pod must be deleted on revoke")
	} else if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
}

func TestEnvtestInvalidRunFails(t *testing.T) {
	e := startEnv(t)
	e.modelSecret(t)
	create(t, e, run("other-cccccccc"))
	e.reconcile(t, "other-cccccccc")
	got := e.get(t, "other-cccccccc")
	if got.Status.Phase != "Failed" || got.Status.FinishedAt == "" || !apimeta.IsStatusConditionTrue(got.Status.Conditions, "Invalid") {
		t.Fatalf("invalid %+v", got.Status)
	}
	if err := e.c.Get(e.ctx, key("other-cccccccc"), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("no pod for an invalid run, got %v", err)
	}
}

func TestEnvtestPermanentCreateFailureEndsTheRun(t *testing.T) {
	e := startEnv(t)
	e.modelSecret(t)
	e.r.Client = cnpFails{e.c, &apimeta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "cilium.io", Kind: "CiliumNetworkPolicy"}}}
	create(t, e, run("run-ffffffff"))
	e.reconcile(t, "run-ffffffff")
	got := e.get(t, "run-ffffffff")
	if got.Status.Phase != "Failed" || got.Status.FinishedAt == "" {
		t.Fatalf("a permanent create failure ends the run: %+v", got.Status)
	}
	if c := apimeta.FindStatusCondition(got.Status.Conditions, "Invalid"); c == nil || c.Status != metav1.ConditionTrue || c.Reason != "CreateFailed" || c.Message == "" {
		t.Fatalf("Invalid condition %+v", c)
	}
	if err := e.c.Get(e.ctx, key("run-ffffffff"), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("no pod after a failed create, got %v", err)
	}
}

func TestEnvtestTransientCreateFailureIsReportedAndRetried(t *testing.T) {
	e := startEnv(t)
	e.modelSecret(t)
	e.r.Client = cnpFails{e.c, apierrors.NewServiceUnavailable("apiserver busy")}
	create(t, e, run("run-gggggggg"))
	if _, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key("run-gggggggg")}); err == nil {
		t.Fatal("a transient create failure must return the error so it is retried")
	}
	got := e.get(t, "run-gggggggg")
	if c := apimeta.FindStatusCondition(got.Status.Conditions, "Ready"); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "CreateFailed" || got.Status.Phase != "Pending" {
		t.Fatalf("Ready condition %+v, status %+v", c, got.Status)
	}
}

func TestEnvtestMissingSecretRequeues(t *testing.T) {
	e := startEnv(t)
	create(t, e, run("run-dddddddd"))
	if res := e.reconcile(t, "run-dddddddd"); res.RequeueAfter == 0 {
		t.Fatal("a missing model Secret must requeue")
	}
	got := e.get(t, "run-dddddddd")
	if got.Status.Phase != "Pending" || apimeta.IsStatusConditionTrue(got.Status.Conditions, "Ready") {
		t.Fatalf("pending without Ready %+v", got.Status)
	}
	if c := apimeta.FindStatusCondition(got.Status.Conditions, "Ready"); c == nil || c.Reason != "SecretMissing" {
		t.Fatalf("Ready condition %+v", c)
	}
}

func TestEnvtestUnsandboxedIsMarked(t *testing.T) {
	e := startEnv(t)
	e.modelSecret(t)
	e.r.Cfg.RuntimeClassName, e.r.Cfg.AllowUnsandboxed = "", true
	create(t, e, run("run-eeeeeeee"))
	e.reconcile(t, "run-eeeeeeee")
	if !apimeta.IsStatusConditionTrue(e.get(t, "run-eeeeeeee").Status.Conditions, "Unsandboxed") {
		t.Fatal("an unsandboxed run must say so")
	}
}

func TestEnvtestStartedRunPodGone(t *testing.T) {
	e := startEnv(t)
	e.modelSecret(t)
	create(t, e, run("run-ffffffff"))
	e.reconcile(t, "run-ffffffff")
	e.markReady(t, "run-ffffffff")
	e.reconcile(t, "run-ffffffff")

	// A stale cache must not end the run: the uncached re-check finds the pod.
	cached := e.r.Client
	e.r.Client = podBlind{cached}
	e.reconcile(t, "run-ffffffff")
	if got := e.get(t, "run-ffffffff"); got.Status.Phase != "Running" {
		t.Fatalf("stale cache ended the run: %+v", got.Status)
	}
	e.r.Client = cached

	p := &corev1.Pod{}
	k := key("run-ffffffff")
	if err := e.c.Get(e.ctx, k, p); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Delete(e.ctx, p, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Get(e.ctx, k, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("pod must be gone (no kubelet to finalise it), got %v", err)
	}
	e.reconcile(t, "run-ffffffff")
	if got := e.get(t, "run-ffffffff"); got.Status.Phase != "Failed" || got.Status.Reason != "PodLost" || got.Status.FinishedAt == "" {
		t.Fatalf("pod lost %+v", got.Status)
	}
	e.reconcile(t, "run-ffffffff")
	if err := e.c.Get(e.ctx, k, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a lost run's pod must not be recreated, got %v", err)
	}
}

func TestEnvtestRunningRunSurvivesConfigGoingInvalid(t *testing.T) {
	e := startEnv(t)
	e.modelSecret(t)
	create(t, e, run("run-gggggggg"))
	e.reconcile(t, "run-gggggggg")
	e.markReady(t, "run-gggggggg")
	e.reconcile(t, "run-gggggggg")

	e.r.Cfg.RuntimeClassName, e.r.Cfg.AllowUnsandboxed = "", false
	e.reconcile(t, "run-gggggggg")
	if got := e.get(t, "run-gggggggg"); got.Status.Phase != "Running" || apimeta.IsStatusConditionTrue(got.Status.Conditions, "Invalid") {
		t.Fatalf("a started run must outlive a config change: %+v", got.Status)
	}
	if err := e.c.Get(e.ctx, key("run-gggggggg"), &corev1.Pod{}); err != nil {
		t.Fatalf("pod must stay: %v", err)
	}
}

func toUnstructured(t *testing.T, r *v1alpha1.AgentRun) *unstructured.Unstructured {
	t.Helper()
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(r)
	if err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("AgentRun"))
	return u
}

func TestEnvtestSameNameRunWaitsForGC(t *testing.T) {
	e := startEnv(t)
	e.modelSecret(t)
	create(t, e, run("run-iiiiiiii"))
	e.reconcile(t, "run-iiiiiiii")
	old := e.get(t, "run-iiiiiiii")
	// envtest has no garbage collector: deleting the run leaves its objects behind, as a slow GC would.
	if err := e.c.Delete(e.ctx, old); err != nil {
		t.Fatal(err)
	}
	create(t, e, run("run-iiiiiiii", func(r *v1alpha1.AgentRun) { r.UID = "" }))
	fresh := e.get(t, "run-iiiiiiii")
	if fresh.UID == old.UID {
		t.Fatal("the recreated run must have a new UID")
	}
	waiting := func(what string) {
		t.Helper()
		if res := e.reconcile(t, "run-iiiiiiii"); res.RequeueAfter == 0 {
			t.Fatalf("%s: must requeue", what)
		}
		got := e.get(t, "run-iiiiiiii")
		if c := apimeta.FindStatusCondition(got.Status.Conditions, "Ready"); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "WaitingForGC" || c.Message == "" {
			t.Fatalf("%s: Ready condition %+v", what, c)
		}
		if got.Status.Phase == "Running" || got.Status.StartedAt != "" {
			t.Fatalf("%s: must not run: %+v", what, got.Status)
		}
	}
	p := &corev1.Pod{}
	waiting("stale pod")
	if err := e.c.Get(e.ctx, key("run-iiiiiiii"), p); err != nil || metav1.GetControllerOf(p).UID != old.UID {
		t.Fatalf("the stale pod must be left alone: %v", err)
	}

	// Only the pod is gone: the stale ServiceAccount, ConfigMaps and policy still block.
	if err := e.c.Delete(e.ctx, p, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	waiting("stale objects, no pod")
	if err := e.c.Get(e.ctx, key("run-iiiiiiii"), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("no pod while a stale owned object remains, got %v", err)
	}

	cnp := &unstructured.Unstructured{}
	cnp.SetAPIVersion("cilium.io/v2")
	cnp.SetKind("CiliumNetworkPolicy")
	cnp.SetNamespace("agents")
	cnp.SetName("run-iiiiiiii")
	for _, o := range []client.Object{&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "run-iiiiiiii", Namespace: "agents"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "run-iiiiiiii-task", Namespace: "agents"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "run-iiiiiiii-ca", Namespace: "agents"}}, cnp} {
		if err := e.c.Delete(e.ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	e.reconcile(t, "run-iiiiiiii")
	for _, o := range []client.Object{&corev1.ServiceAccount{}, &corev1.Pod{}, &corev1.ConfigMap{}} {
		n := "run-iiiiiiii"
		if _, ok := o.(*corev1.ConfigMap); ok {
			n += "-task"
		}
		if err := e.c.Get(e.ctx, key(n), o); err != nil {
			t.Fatalf("%T: %v", o, err)
		}
		if ref := metav1.GetControllerOf(o); ref == nil || ref.UID != fresh.UID {
			t.Fatalf("%T is not controlled by the new run: %+v", o, ref)
		}
	}
	if c := apimeta.FindStatusCondition(e.get(t, "run-iiiiiiii").Status.Conditions, "Ready"); c == nil || c.Status != metav1.ConditionTrue || c.Reason != "Created" {
		t.Fatalf("Ready condition %+v", c)
	}
}

func TestEnvtestPendingPodLostIsNotRecreated(t *testing.T) {
	e := startEnv(t)
	e.modelSecret(t)
	create(t, e, run("run-jjjjjjjj"))
	e.reconcile(t, "run-jjjjjjjj")
	if got := e.get(t, "run-jjjjjjjj"); got.Status.Phase != "Pending" || got.Status.StartedAt != "" {
		t.Fatalf("pod pending: %+v", got.Status)
	}
	p := &corev1.Pod{}
	if err := e.c.Get(e.ctx, key("run-jjjjjjjj"), p); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Delete(e.ctx, p, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t, "run-jjjjjjjj")
	if got := e.get(t, "run-jjjjjjjj"); got.Status.Phase != "Failed" || got.Status.Reason != "PodLost" || got.Status.FinishedAt == "" {
		t.Fatalf("pending pod lost: %+v", got.Status)
	}
	if err := e.c.Get(e.ctx, key("run-jjjjjjjj"), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the agent must not run twice, got %v", err)
	}
}

// HR4: the factory's reviewer claim, as runs.Build writes it, carries its brief beside the pull
// request; the CRD admits it, and the run's task.md is the brief, not "Work on <url>.". Every other
// role still sets exactly one of the two.
func TestEnvtestAReviewerClaimCarriesItsBriefBesideThePullRequest(t *testing.T) {
	e := startEnv(t)
	e.modelSecret(t)
	const pr = "https://github.com/o/r/pull/12"
	claim := func(name, role, text, url string) *unstructured.Unstructured {
		u := runs.Build(runs.Spec{RunID: name[len("run-"):], Role: role, Repository: "o/r", BaseRef: "main",
			Branch: "agent/3buqdlot", TaskText: text, TaskURL: url, Principal: runs.PrincipalFactory, DataClass: "public",
			Model: "agent-default", MaxTokens: 1000, MaxMinutes: 30})
		u.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("AgentRun"))
		u.SetNamespace("agents")
		u.SetName(name)
		return u
	}
	if err := e.c.Create(e.ctx, claim("run-rvwrrvwr", "reviewer", "You are the reviewer\n", pr)); err != nil {
		t.Fatalf("a reviewer claim with its brief: %v", err)
	}
	e.reconcile(t, "run-rvwrrvwr")
	cm := &corev1.ConfigMap{}
	if err := e.c.Get(e.ctx, key("run-rvwrrvwr-task"), cm); err != nil || cm.Data["task.md"] != "You are the reviewer\n" {
		t.Fatalf("task.md %q, %v", cm.Data["task.md"], err)
	}
	p := &corev1.Pod{}
	if err := e.c.Get(e.ctx, key("run-rvwrrvwr"), p); err != nil {
		t.Fatal(err)
	}
	var taskURL string
	for _, c := range p.Spec.Containers {
		for _, v := range c.Env {
			if c.Name == "harness" && v.Name == "TASK_URL" {
				taskURL = v.Value
			}
		}
	}
	if taskURL != pr {
		t.Fatalf("TASK_URL %q, want the pull request", taskURL)
	}
	both := claim("run-mplmmplm", "implementer", "fix it", pr)
	if err := unstructured.SetNestedStringMap(both.Object, map[string]string{"text": "fix it", "url": pr}, "spec", "task"); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Create(e.ctx, both); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "only a reviewer run carries both") {
		t.Fatalf("an implementer with both task fields: %v, want Invalid", err)
	}
	none := claim("run-nnnnnnnn", "implementer", "x", "")
	if err := unstructured.SetNestedStringMap(none.Object, map[string]string{}, "spec", "task"); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Create(e.ctx, none); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "set task.text, task.url, or both") {
		t.Fatalf("a run with no task: %v, want Invalid", err)
	}
}
