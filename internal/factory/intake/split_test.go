// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/forge/forgetest"
	"github.com/Smana/agent-platform/internal/factory/taskid"
)

func stopOf(t *testing.T, p *IssuePoller, name string) string {
	t.Helper()
	var tk v1alpha1.Task
	_ = p.Client.Get(t.Context(), types.NamespacedName{Namespace: "agent-system", Name: name}, &tk)
	return tk.Annotations[v1alpha1.AnnotationStop]
}

// Splitter design SL13, offline proof 6: factory/stop on the parent issue stops the parent and its
// running part; a finished part is left alone.
func TestAStopOnTheParentStopsTheFamily(t *testing.T) {
	parent := &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "3buqdlot", Namespace: "agent-system"},
		Spec: v1alpha1.TaskSpec{Issue: 7}, Status: v1alpha1.TaskStatus{Phase: v1alpha1.PhaseSplit}}
	running := &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "chldtwoo", Namespace: "agent-system"},
		Spec:   v1alpha1.TaskSpec{Issue: 502, Parent: &v1alpha1.Parent{Task: "3buqdlot", Issue: 7, Index: 2, Of: 2}},
		Status: v1alpha1.TaskStatus{Phase: v1alpha1.PhaseImplementing}}
	landed := &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "chldonee", Namespace: "agent-system"},
		Spec:   v1alpha1.TaskSpec{Issue: 501, Parent: &v1alpha1.Parent{Task: "3buqdlot", Issue: 7, Index: 1, Of: 2}},
		Status: v1alpha1.TaskStatus{Phase: v1alpha1.PhaseDone}}
	f := forgetest.NewFake()
	f.SetLabeled(LabelStop, forge.Item{Number: 7})
	f.SetEvents(7, forge.LabelEvent{Actor: "Smana", Label: LabelStop, At: t0})
	p, _ := poller(t, f, parent, running, landed)
	if err := p.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"3buqdlot": "label", "chldtwoo": "label", "chldonee": ""} {
		if got := stopOf(t, p, name); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
}

// R4, offline proof 6: factory/ready on a parent waiting in Split is refused task_active.
func TestTheParentCannotFanOutTwice(t *testing.T) {
	parent := &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "3buqdlot", Namespace: "agent-system",
		Labels: map[string]string{v1alpha1.LabelIssue: "7"}}, Spec: v1alpha1.TaskSpec{Issue: 7, Repository: "Smana/cloud-native-ref"},
		Status: v1alpha1.TaskStatus{Phase: v1alpha1.PhaseSplit}}
	f := forgetest.NewFake()
	labelled(f, 7, "Smana")
	p, c := poller(t, f, parent)
	if err := p.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := len(tasks(t, c)); n != 1 || len(f.Comments(7)) != 1 {
		t.Fatalf("%d tasks, comments %q", n, f.Comments(7))
	}
}

// SL12, SR16, offline proof 6: on a part's issue, a child-of label a user applied links nothing; the
// factory App's links the new generation to its parent, copied from the earlier generation.
func TestARelabelledPartJoinsItsFamily(t *testing.T) {
	for name, c := range map[string]struct {
		actor  string
		linked bool
	}{
		"the factory's label": {"ogenki-agent-factory[bot]", true},
		"a user's label":      {"octocat", false},
	} {
		t.Run(name, func(t *testing.T) {
			first := &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: taskid.Name(taskid.SplitKey("3buqdlot", 1)), Namespace: "agent-system",
				Labels: map[string]string{v1alpha1.LabelIssue: "501", v1alpha1.LabelParent: "3buqdlot"}},
				Spec:   v1alpha1.TaskSpec{Issue: 501, Repository: "Smana/cloud-native-ref", Parent: &v1alpha1.Parent{Task: "3buqdlot", Issue: 7, Index: 1, Of: 2}},
				Status: v1alpha1.TaskStatus{Phase: v1alpha1.PhaseStopped}}
			f := forgetest.NewFake()
			labelled(f, 501, "Smana")
			f.SetIssue(forge.Issue{Number: 501, Title: "Part 1 of 2 of #7", Body: "…", Labels: []string{"factory/child-of:#7", "factory/ready"}})
			f.SetEvents(501, forge.LabelEvent{Actor: c.actor, Label: "factory/child-of:#7", At: t0.Add(-time.Hour)},
				forge.LabelEvent{Actor: "Smana", Label: "factory/ready", At: t0})
			p, cl := poller(t, f, first)
			if err := p.Poll(t.Context()); err != nil {
				t.Fatal(err)
			}
			var tk v1alpha1.Task
			if err := cl.Get(t.Context(), types.NamespacedName{Namespace: "agent-system",
				Name: taskid.Name(taskid.IssueKey("Smana/cloud-native-ref", 501, 1))}, &tk); err != nil {
				t.Fatal(err)
			}
			if linked := tk.Spec.Parent != nil && *tk.Spec.Parent == *first.Spec.Parent && tk.Labels[v1alpha1.LabelParent] == "3buqdlot"; linked != c.linked {
				t.Fatalf("parent %+v labels %v", tk.Spec.Parent, tk.Labels)
			}
		})
	}
}
