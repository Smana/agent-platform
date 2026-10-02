// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

// R51: a cluster rebuild loses Tasks, not their pull requests. An open PR from an agent branch,
// carrying the class label, with no Task of that name, is the factory's orphan: labelled
// factory/orphaned and narrated once — it is human-only now, and the restart is a fresh
// factory/ready, never an adoption (R52: the branch and the footer are forgeable hints).
func TestOrphanedPullRequestsAreLabelledOnce(t *testing.T) {
	f := forge.NewFake()
	f.SetAgentPulls(
		forge.AgentPull{Number: 21, HeadRef: "agent/abcdefgh", Labels: []string{"factory/class:review"}, Fork: true},
		forge.AgentPull{Number: 22, HeadRef: "agent/bbbbbbbb", Labels: []string{"factory/class:review"}},
		forge.AgentPull{Number: 23, HeadRef: "agent/waytoolong", Labels: []string{"factory/class:review"}},
		forge.AgentPull{Number: 24, HeadRef: "agent/cccccccc", Labels: []string{"bug"}},
	)
	// A task that still lives, of a pull request the scan never sees: it is no orphan.
	live := &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "dddddddd", Namespace: "agent-system"},
		Status: v1alpha1.TaskStatus{Phase: v1alpha1.PhaseDone}}
	p, _ := poller(t, f, live)
	for range 2 { // every issues poll, leader start included
		if err := p.Poll(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.Added(22); !slices.Equal(got, []string{LabelOrphaned}) {
		t.Fatalf("the own-repo orphan with no Task is labelled once: %v", got)
	}
	if c := f.Comments(22); len(c) != 1 || !strings.Contains(c[0], "lost in a cluster rebuild") ||
		!strings.Contains(c[0], "factory/ready") {
		t.Fatalf("narrated once, with the restart: %q", c)
	}
	for _, n := range []int{21, 23, 24} {
		if got := f.Added(n); len(got) != 0 || len(f.Comments(n)) != 0 {
			t.Fatalf("#%d is not the factory's to judge: %v %q", n, got, f.Comments(n))
		}
	}
	if n := len(tasks(t, p.Client)); n != 1 {
		t.Fatalf("the scan starts nothing: %d tasks", n)
	}
}

// The narration is the marker's to dedup, not the label's: a pull request that already carries
// factory/orphaned is not labelled again, and the comment is not repeated.
func TestAnOrphanIsNotRelabelledOrRenarrated(t *testing.T) {
	f := forge.NewFake()
	f.SetAgentPulls(forge.AgentPull{Number: 22, HeadRef: "agent/bbbbbbbb", Labels: []string{"factory/class:review"}})
	p, _ := poller(t, f)
	for range 2 {
		if err := p.Poll(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.Added(22); !slices.Equal(got, []string{LabelOrphaned}) {
		t.Fatalf("labelled once: %v", got)
	}
	if c := f.Comments(22); len(c) != 1 {
		t.Fatalf("narrated once, by the marker: %q", c)
	}
}
