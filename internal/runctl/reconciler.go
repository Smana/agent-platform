// SPDX-License-Identifier: Apache-2.0

package runctl

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha1 "github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/runwatch"
)

const secretRetry = 30 * time.Second

// Reconciler turns an AgentRun into its owned objects and reports the pod back as status.
// Owned objects are created once: a run's spec is immutable, so there is nothing to patch.
type Reconciler struct {
	Client    client.Client
	APIReader client.Reader // uncached: Secrets are never listed or watched, and a cached pod NotFound is never trusted
	Cfg       Config
	Now       func() time.Time
	Log       *slog.Logger
}

// SetupWithManager watches runs and the pods they own.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("run-controller").
		For(&v1alpha1.AgentRun{}).Owns(&corev1.Pod{}).Complete(r)
}

// Reconcile is idempotent: a restart reconciles every non-terminal run from scratch.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	run := &v1alpha1.AgentRun{}
	if err := r.Client.Get(ctx, req.NamespacedName, run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if runwatch.Terminal(run.Status.Phase) {
		return ctrl.Result{}, nil
	}
	now := r.Now()
	st := run.Status.DeepCopy()
	if why := r.Cfg.Invalid(run); why != "" {
		st.Phase, st.FinishedAt = "Failed", now.UTC().Format(time.RFC3339)
		apimeta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: "Invalid", Status: metav1.ConditionTrue, Reason: "Refused", Message: why})
		return ctrl.Result{}, r.writeStatus(ctx, run, *st)
	}
	runID := r.Cfg.RunID(run)
	st.RunID, st.Branch, st.ConversationID = runID, Branch(run, runID), string(run.UID)
	revoked := run.Annotations[runwatch.RevokedAnnotation]

	pod, gone, err := r.pod(ctx, run)
	if err != nil {
		return ctrl.Result{}, err
	}
	if RevokeEnds(revoked) {
		if pod != nil && pod.DeletionTimestamp == nil {
			if err := r.Client.Delete(ctx, pod); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, r.writeStatus(ctx, run, merge(*st, Observe(run.Status, revoked, pod, gone, now)))
	}
	if pod == nil && !gone {
		m, ca, missing, err := r.inputs(ctx)
		if err != nil {
			return ctrl.Result{}, err
		}
		if missing != "" {
			apimeta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "SecretMissing", Message: missing})
			if st.Phase == "" {
				st.Phase = "Pending"
			}
			return ctrl.Result{RequeueAfter: secretRetry}, r.writeStatus(ctx, run, *st)
		}
		if err := r.create(ctx, run, runID, m, ca); err != nil {
			return ctrl.Result{}, err
		}
		apimeta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Created"})
		if r.Cfg.RuntimeClassName == "" {
			apimeta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: "Unsandboxed", Status: metav1.ConditionTrue, Reason: "AllowUnsandboxed",
				Message: "the pod runs without a RuntimeClass: allowUnsandboxed is set"})
		}
	}
	return ctrl.Result{}, r.writeStatus(ctx, run, merge(*st, Observe(run.Status, revoked, pod, gone, now)))
}

// pod is the run's pod; gone is that it existed (the run has a startedAt) and does not now.
func (r *Reconciler) pod(ctx context.Context, run *v1alpha1.AgentRun) (*corev1.Pod, bool, error) {
	p := &corev1.Pod{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Name}, p)
	if apierrors.IsNotFound(err) {
		// Observe ends a started run as PodLost on nil: confirm the cache is not just behind.
		err = r.APIReader.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Name}, p)
		if apierrors.IsNotFound(err) {
			return nil, run.Status.StartedAt != "", nil
		}
	}
	if err != nil {
		return nil, false, err
	}
	return p, false, nil
}

// inputs reads the model Secret, the git Secret's presence and the broker CA. missing names
// the first absent one: the chart may create them after the controller starts.
func (r *Reconciler) inputs(ctx context.Context) (Model, []byte, string, error) {
	get := func(ns, name string) (*corev1.Secret, error) {
		s := &corev1.Secret{}
		return s, r.APIReader.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, s)
	}
	ms, err := get(r.Cfg.Namespace, r.Cfg.ModelSecret)
	if apierrors.IsNotFound(err) {
		return Model{}, nil, fmt.Sprintf("Secret %s/%s not found", r.Cfg.Namespace, r.Cfg.ModelSecret), nil
	} else if err != nil {
		return Model{}, nil, "", err
	}
	m, err := ModelFrom(ms.Data)
	if err != nil {
		return Model{}, nil, err.Error(), nil
	}
	if gs, err := get(r.Cfg.Namespace, r.Cfg.GitSecret); apierrors.IsNotFound(err) || err == nil && len(gs.Data["token"]) == 0 {
		return Model{}, nil, fmt.Sprintf("Secret %s/%s has no token", r.Cfg.Namespace, r.Cfg.GitSecret), nil
	} else if err != nil {
		return Model{}, nil, "", err
	}
	cs, err := get(r.Cfg.Broker.Namespace, r.Cfg.Broker.CASecret)
	if apierrors.IsNotFound(err) || err == nil && len(cs.Data["ca.crt"]) == 0 {
		return Model{}, nil, fmt.Sprintf("Secret %s/%s has no ca.crt", r.Cfg.Broker.Namespace, r.Cfg.Broker.CASecret), nil
	} else if err != nil {
		return Model{}, nil, "", err
	}
	return m, cs.Data["ca.crt"], "", nil
}

// create makes every owned object, the pod last so its volumes and ServiceAccount exist.
func (r *Reconciler) create(ctx context.Context, run *v1alpha1.AgentRun, runID string, m Model, ca []byte) error {
	task, err := TaskConfigMap(run, runID)
	if err != nil {
		return err
	}
	for _, o := range []client.Object{ServiceAccount(run), task, CAConfigMap(run, ca),
		NetworkPolicy(run, r.Cfg, m, runID), Pod(run, r.Cfg, m, runID)} {
		if err := controllerutil.SetControllerReference(run, o, r.Client.Scheme()); err != nil {
			return err
		}
		if err := r.Client.Create(ctx, o); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create %T %s: %w", o, o.GetName(), err)
		}
	}
	return nil
}

// merge lays Observe's phase, reason and times over the reconciler's identity fields and conditions.
func merge(base, obs v1alpha1.AgentRunStatus) v1alpha1.AgentRunStatus {
	base.Phase, base.Reason, base.StartedAt, base.FinishedAt = obs.Phase, obs.Reason, obs.StartedAt, obs.FinishedAt
	return base
}

func (r *Reconciler) writeStatus(ctx context.Context, run *v1alpha1.AgentRun, st v1alpha1.AgentRunStatus) error {
	if equality.Semantic.DeepEqual(run.Status, st) {
		return nil
	}
	run.Status = st
	return r.Client.Status().Update(ctx, run)
}
