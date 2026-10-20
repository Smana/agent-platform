// SPDX-License-Identifier: Apache-2.0

package runctl

import (
	"context"
	"errors"
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

const (
	secretRetry = 30 * time.Second
	gcRetry     = 5 * time.Second
)

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
	revoked := run.Annotations[runwatch.RevokedAnnotation]

	pod, gone, stale, err := r.pod(ctx, run)
	if err != nil {
		return ctrl.Result{}, err
	}
	if stale != "" {
		return r.waitForGC(ctx, run, st, stale)
	}
	// Invalid depends on config, which can change across a restart: a run that already
	// has a pod must not be failed with its pod left running.
	if pod == nil && run.Status.StartedAt == "" {
		if why := r.Cfg.Invalid(run); why != "" {
			fail(st, now, "Refused", why)
			return ctrl.Result{}, r.writeStatus(ctx, run, *st)
		}
	}
	runID := r.Cfg.RunID(run)
	st.RunID, st.Branch, st.ConversationID = runID, Branch(run, runID), string(run.UID)
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
			pending(st, "SecretMissing", missing)
			return ctrl.Result{RequeueAfter: secretRetry}, r.writeStatus(ctx, run, *st)
		}
		stale, err := r.create(ctx, run, runID, m, ca)
		if stale != "" {
			return r.waitForGC(ctx, run, st, stale)
		}
		if err != nil {
			// A refusal that retrying cannot cure (a bad object, a forbidden verb, a missing
			// CRD) ends the run so the broker stops waiting; anything else is retried.
			if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) || apimeta.IsNoMatchError(err) {
				fail(st, now, "CreateFailed", err.Error())
				setCond(st, "Ready", metav1.ConditionFalse, "CreateFailed", err.Error())
				return ctrl.Result{}, r.writeStatus(ctx, run, *st)
			}
			pending(st, "CreateFailed", err.Error())
			if werr := r.writeStatus(ctx, run, *st); werr != nil {
				return ctrl.Result{}, errors.Join(err, werr)
			}
			return ctrl.Result{}, err
		}
		setCond(st, "Ready", metav1.ConditionTrue, "Created", "")
		if r.Cfg.RuntimeClassName == "" {
			setCond(st, "Unsandboxed", metav1.ConditionTrue, "AllowUnsandboxed", "the pod runs without a RuntimeClass: allowUnsandboxed is set")
		}
	}
	return ctrl.Result{}, r.writeStatus(ctx, run, merge(*st, Observe(run.Status, revoked, pod, gone, now)))
}

// waitForGC holds a run whose name is still taken by the objects of a deleted run of the
// same name (run ids are deterministic): reusing them would run the old task.
func (r *Reconciler) waitForGC(ctx context.Context, run *v1alpha1.AgentRun, st *v1alpha1.AgentRunStatus, what string) (ctrl.Result, error) {
	pending(st, "WaitingForGC", what+" belongs to a previous run of the same name; waiting for its garbage collection")
	return ctrl.Result{RequeueAfter: gcRetry}, r.writeStatus(ctx, run, *st)
}

// ours reports whether o is controlled by this very run, not by an earlier one of the same name.
func ours(run *v1alpha1.AgentRun, o client.Object) bool {
	ref := metav1.GetControllerOf(o)
	return ref != nil && ref.UID == run.UID
}

// hadPod is that the run's pod was created: a missing pod is then lost, never re-created.
func hadPod(run *v1alpha1.AgentRun) bool {
	return run.Status.StartedAt != ""
}

// pod is the run's pod; gone is that it existed (hadPod) and does not now. stale names a pod
// of that name controlled by another run, which is neither observed nor replaced.
func (r *Reconciler) pod(ctx context.Context, run *v1alpha1.AgentRun) (p *corev1.Pod, gone bool, stale string, err error) {
	k := types.NamespacedName{Namespace: run.Namespace, Name: run.Name}
	p = &corev1.Pod{}
	err = r.Client.Get(ctx, k, p)
	if apierrors.IsNotFound(err) {
		// Observe ends a run that had a pod as PodLost on nil: confirm the cache is not just behind.
		err = r.APIReader.Get(ctx, k, p)
		if apierrors.IsNotFound(err) {
			return nil, hadPod(run), "", nil
		}
	}
	if err != nil {
		return nil, false, "", err
	}
	if !ours(run, p) {
		return nil, false, "pod " + p.Name, nil
	}
	return p, false, "", nil
}

// setCond sets one status condition.
func setCond(st *v1alpha1.AgentRunStatus, typ string, status metav1.ConditionStatus, reason, msg string) {
	apimeta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: typ, Status: status, Reason: reason, Message: msg})
}

// fail ends the run: phase Failed, finishedAt now, condition Invalid.
func fail(st *v1alpha1.AgentRunStatus, now time.Time, reason, msg string) {
	st.Phase, st.FinishedAt = "Failed", now.UTC().Format(time.RFC3339)
	setCond(st, "Invalid", metav1.ConditionTrue, reason, msg)
}

// pending marks the run not Ready, and Pending unless it already has a phase.
func pending(st *v1alpha1.AgentRunStatus, reason, msg string) {
	setCond(st, "Ready", metav1.ConditionFalse, reason, msg)
	if st.Phase == "" {
		st.Phase = "Pending"
	}
}

// secretKey reads key from a Secret. missing is true when the Secret or the key (empty
// counts) is absent; the caller words it.
func (r *Reconciler) secretKey(ctx context.Context, ns, name, key string) (val []byte, missing bool, err error) {
	s := &corev1.Secret{}
	err = r.APIReader.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, s)
	if apierrors.IsNotFound(err) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	return s.Data[key], len(s.Data[key]) == 0, nil
}

// inputs reads the model Secret, the git Secret's presence and the broker CA. missing names
// the first absent one: the chart may create them after the controller starts.
func (r *Reconciler) inputs(ctx context.Context) (Model, []byte, string, error) {
	c := r.Cfg
	ms := &corev1.Secret{}
	err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: c.Namespace, Name: c.ModelSecret}, ms)
	if apierrors.IsNotFound(err) {
		return Model{}, nil, fmt.Sprintf("Secret %s/%s not found", c.Namespace, c.ModelSecret), nil
	} else if err != nil {
		return Model{}, nil, "", err
	}
	m, err := ModelFrom(ms.Data)
	if err != nil {
		return Model{}, nil, err.Error(), nil
	}
	if _, missing, err := r.secretKey(ctx, c.Namespace, c.GitSecret, "token"); err != nil {
		return Model{}, nil, "", err
	} else if missing {
		return Model{}, nil, fmt.Sprintf("Secret %s/%s has no token", c.Namespace, c.GitSecret), nil
	}
	ca, missing, err := r.secretKey(ctx, c.Broker.Namespace, c.Broker.CASecret, "ca.crt")
	if err != nil {
		return Model{}, nil, "", err
	} else if missing {
		return Model{}, nil, fmt.Sprintf("Secret %s/%s has no ca.crt", c.Broker.Namespace, c.Broker.CASecret), nil
	}
	return m, ca, "", nil
}

// create makes every owned object, the pod last so its volumes and ServiceAccount exist.
// stale names an existing object that another run controls: nothing after it is created.
func (r *Reconciler) create(ctx context.Context, run *v1alpha1.AgentRun, runID string, m Model, ca []byte) (stale string, err error) {
	task, err := TaskConfigMap(run, runID)
	if err != nil {
		return "", err
	}
	for _, o := range []client.Object{ServiceAccount(run), task, CAConfigMap(run, ca),
		NetworkPolicy(run, r.Cfg, m, runID), Pod(run, r.Cfg, m, runID)} {
		if err := controllerutil.SetControllerReference(run, o, r.Client.Scheme()); err != nil {
			return "", err
		}
		err := r.Client.Create(ctx, o)
		if apierrors.IsAlreadyExists(err) {
			existing := o.DeepCopyObject().(client.Object)
			if err = r.APIReader.Get(ctx, client.ObjectKeyFromObject(o), existing); err == nil && !ours(run, existing) {
				return fmt.Sprintf("%T %s", o, o.GetName()), nil
			}
		}
		if err != nil {
			return "", fmt.Errorf("create %T %s: %w", o, o.GetName(), err)
		}
	}
	return "", nil
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
