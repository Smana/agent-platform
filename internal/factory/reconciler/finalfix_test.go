// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	roomv1 "github.com/Smana/maku/api/v1alpha1"
	"github.com/Smana/maku/internal/factory/config"
)

// failRoomCreate makes the apiserver refuse a Room create with err.
func failRoomCreate(g *rig, err error) {
	g.r.Client = interceptor.NewClient(g.c.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, o ...client.CreateOption) error {
			if _, ok := obj.(*roomv1.Room); ok {
				return err
			}
			return cl.Create(ctx, obj, o...)
		}})
}

// An old Rooms CRD refuses a system: member as Invalid: the task escalates with a reason an
// operator can act on, instead of retrying in Triaged for good.
func TestAnOldRoomsCRDEscalatesTheTask(t *testing.T) {
	g := sponsoredDayRig(t, issueTask("3buqdlot", 0, "Fix the dead link in docs/a.md."))
	failRoomCreate(g, apierrors.NewInvalid(schema.GroupKind{Group: "agents.ogenki.io", Kind: "Room"}, "3buqdlot", nil))
	got := g.reconcile(t, "3buqdlot", 4)
	if got.Status.Phase != v1alpha1.PhaseEscalated || got.Status.Reason != "rooms_crd_outdated" || len(g.runs.specs) != 0 {
		t.Fatalf("%s %s, %d specs", got.Status.Phase, got.Status.Reason, len(g.runs.specs))
	}
}

// Any other refusal keeps retrying: the task stays Triaged and the reconcile returns the error.
func TestAnotherRoomErrorKeepsRetrying(t *testing.T) {
	g := sponsoredDayRig(t, issueTask("3buqdlot", 0, "Fix the dead link in docs/a.md."))
	failRoomCreate(g, apierrors.NewServiceUnavailable("etcd"))
	var failed bool
	for range 5 {
		if _, err := g.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "agent-system", Name: "3buqdlot"}}); err != nil {
			failed = true
		}
	}
	got := g.reconcile(t, "3buqdlot", 0)
	if !failed || got.Status.Phase != v1alpha1.PhaseTriaged {
		t.Fatalf("failed %v, phase %s %s", failed, got.Status.Phase, got.Status.Reason)
	}
}

// An api task's scoper run is capped by the task's own ceiling-derived run tokens, never above
// the light tier's.
func TestAnAPITasksScoperIsCappedByItsCeiling(t *testing.T) {
	g := newRig(t)
	g.r.Cfg.Tiers["light"] = config.Tier{Model: "agent-default", RunTokens: 300_000, TaskTokens: 600_000, RunMinutes: 20}
	for name, c := range map[string]struct {
		api       bool
		runTokens int64
		want      int64
	}{
		"api task under the tier": {true, 120_000, 120_000},
		"api task over the tier":  {true, 900_000, 300_000},
		"issue task is untouched": {false, 120_000, 300_000},
	} {
		t.Run(name, func(t *testing.T) {
			tk := issueTask("3buqdlot", 0, "x")
			if c.api {
				sponsor(tk, 2_000_000)
			}
			tk.Spec.Budget.RunTokens = c.runTokens
			if s := g.r.scoperSpec(tk, "text"); s.MaxTokens != c.want {
				t.Fatalf("MaxTokens %d, want %d", s.MaxTokens, c.want)
			}
		})
	}
}

// A titled api task's first brief says how it arrived, never that a maintainer labelled it.
func TestATitledAPITaskBriefNamesTheAPI(t *testing.T) {
	tk := issueTask("3buqdlot", 7, "fix it")
	sponsor(tk, 2_000_000)
	b := FirstBrief(tk, "n0nce234")
	if strings.Contains(b, "labelled for the factory") || !strings.Contains(b, "requested through the API by system:slack-app for github:smana") ||
		!strings.Contains(b, `"Fixes #7"`) {
		t.Fatal(b)
	}
}
