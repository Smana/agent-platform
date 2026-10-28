// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	factoryv1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/authn"
)

const (
	maxEvidenceBytes     = 16 << 10
	maxVerificationBody  = 32 << 10
	verificationAttempts = 3
)

// verificationRequest is POST /v1/tasks/{task}/verification's body (RA11): RunLore's verdict on
// the fix its proposal led to, and the evidence behind it.
type verificationRequest struct {
	Verdict string `json:"verdict"`
	Alert   struct {
		Name          string `json:"name"`
		FiringAtMerge bool   `json:"firingAtMerge"`
		FiringNow     bool   `json:"firingNow"`
	} `json:"alert"`
	CheckedAt time.Time `json:"checkedAt"`
	Evidence  string    `json:"evidence"`
}

// anyVerifies reports whether an internal principal may send verdicts: the route is a 404 otherwise (D4).
func (s *Server) anyVerifies() bool {
	for _, ip := range s.Cfg.API.InternalPrincipals {
		if ip.Verification {
			return true
		}
	}
	return false
}

// recordVerification is POST /v1/tasks/{task}/verification (RA11, R11): an internal principal says
// whether the fix of its own proposal held. The verdict lands in the fix's status.monitor, which
// the post-merge monitor reads; the evidence lands on the origin task, which stays internal.
func (s *Server) recordVerification(w http.ResponseWriter, r *http.Request) {
	p, err := s.Auth.Authenticate(r)
	switch {
	case errors.Is(err, authn.ErrForbidden):
		refuse(w, http.StatusForbidden, "not_permitted")
		return
	case err != nil:
		refuse(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	ip := s.Cfg.InternalBy(p.ID)
	switch {
	case !p.Internal || ip == nil:
		refuse(w, http.StatusForbidden, "not_permitted")
		return
	case !ip.Verification:
		refuse(w, http.StatusForbidden, "verification_not_allowed")
		return
	}
	var in verificationRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxVerificationBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || in.CheckedAt.IsZero() || in.Alert.Name == "" || len(in.Alert.Name) > 256 {
		refuse(w, http.StatusBadRequest, "bad_json")
		return
	}
	if in.Verdict != "resolved" && in.Verdict != "persisting" {
		refuse(w, http.StatusBadRequest, "bad_verdict")
		return
	}
	if len(in.Evidence) > maxEvidenceBytes || s.Redact == nil {
		refuse(w, http.StatusBadRequest, "bad_evidence")
		return
	}
	if _, rules := s.Redact.String(in.Evidence); len(rules) > 0 {
		refuse(w, http.StatusBadRequest, "bad_evidence")
		return
	}
	ctx := r.Context()
	unavailable := func(err error) {
		if s.Log != nil {
			s.Log.ErrorContext(ctx, "verification: "+err.Error(), "principal", p.ID)
		}
		refuse(w, http.StatusServiceUnavailable, "tasks_unavailable")
	}
	key := types.NamespacedName{Namespace: s.Namespace, Name: r.PathValue("task")}
	for attempt := 1; ; attempt++ {
		var t factoryv1.Task
		switch err := s.Rooms.Get(ctx, key, &t); {
		case apierrors.IsNotFound(err):
			refuse(w, http.StatusNotFound, "no_such_task")
			return
		case err != nil:
			unavailable(err)
			return
		}
		var origin factoryv1.Task
		if t.Spec.Origin == "" {
			refuse(w, http.StatusForbidden, "not_own_task")
			return
		}
		switch err := s.Rooms.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: t.Spec.Origin}, &origin); {
		case apierrors.IsNotFound(err):
			refuse(w, http.StatusForbidden, "not_own_task")
			return
		case err != nil:
			unavailable(err)
			return
		}
		if origin.Spec.Source.RequestedBy != p.ID {
			refuse(w, http.StatusForbidden, "not_own_task")
			return
		}
		pr := t.Status.PullRequest
		if pr == nil || pr.MergedAt == nil {
			refuse(w, http.StatusConflict, "not_merged")
			return
		}
		if v := recorded(&t); v != nil {
			// A replay is answered before the window: it never reads as a refusal. It also completes
			// the evidence a first call could not write after its verdict.
			if v.Verdict != in.Verdict {
				refuse(w, http.StatusConflict, "verdict_already_recorded")
				return
			}
			if err := s.writeEvidence(ctx, &t, &origin, in.Evidence); err != nil {
				unavailable(err)
				return
			}
			reply(w, http.StatusOK, map[string]any{"task": t.Name, "verdict": v.Verdict, "recordedAt": v.RecordedAt.Time, "duplicate": true})
			return
		}
		vw := s.Cfg.RepositoryFor(t.Spec.Repository).Classes[t.Spec.PredictedClass].Monitor
		// One window with the monitor: its Since is GitHub's merge time once a pass has read it; until
		// then the Task's MergedAt, the factory's clock, which the monitor's first pass corrects.
		since := pr.MergedAt.Time
		if t.Status.Monitor != nil {
			since = t.Status.Monitor.Since.Time
		}
		switch {
		case vw == nil || vw.Verification == nil:
			refuse(w, http.StatusForbidden, "not_own_task")
			return
		case s.Now().After(since.Add(vw.Verification.Within.Duration)):
			refuse(w, http.StatusConflict, "window_closed")
			return
		}
		at := metav1.NewTime(s.Now())
		err := s.writeVerification(ctx, &t, &origin, in, p.ID, at)
		switch {
		case err == nil:
			reply(w, http.StatusCreated, map[string]any{"task": t.Name, "verdict": in.Verdict, "recordedAt": at.Time})
			return
		case apierrors.IsConflict(err) && attempt < verificationAttempts:
			continue // somebody wrote the task meanwhile: read it again, its verdict may be there
		default:
			unavailable(err)
			return
		}
	}
}

func recorded(t *factoryv1.Task) *factoryv1.Verification {
	if t.Status.Monitor == nil {
		return nil
	}
	return t.Status.Monitor.Verification
}

// writeVerification writes the verdict first, under the task's resourceVersion so two racing
// verdicts cannot both land, and the evidence only after it: a loser never leaves evidence behind.
// A failure between the two leaves a replay to complete the evidence (see recordVerification).
func (s *Server) writeVerification(ctx context.Context, t, origin *factoryv1.Task, in verificationRequest, by string, at metav1.Time) error {
	base := t.DeepCopy()
	if t.Status.Monitor == nil {
		t.Status.Monitor = &factoryv1.MonitorStatus{Since: *t.Status.PullRequest.MergedAt}
	}
	t.Status.Monitor.Verification = &factoryv1.Verification{Verdict: in.Verdict, CheckedAt: metav1.NewTime(in.CheckedAt), RecordedAt: at, By: by,
		Alert: factoryv1.VerifiedAlert{Name: in.Alert.Name, FiringAtMerge: in.Alert.FiringAtMerge, FiringNow: in.Alert.FiringNow}}
	if err := s.Rooms.Status().Patch(ctx, t, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	return s.writeEvidence(ctx, t, origin, in.Evidence)
}

// writeEvidence puts the evidence on the origin task unless it already holds this fix's.
func (s *Server) writeEvidence(ctx context.Context, t, origin *factoryv1.Task, evidence string) error {
	if evidence == "" || origin.Status.Published == nil || origin.Status.Published.EvidenceFor == t.Name {
		return nil
	}
	base := origin.DeepCopy()
	origin.Status.Published.Evidence, origin.Status.Published.EvidenceFor = evidence, t.Name
	return s.Rooms.Status().Patch(ctx, origin, client.MergeFrom(base))
}
