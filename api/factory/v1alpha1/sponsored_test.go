// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "testing"

// R7: an api task is charged to its principal; a maintainer's retry moves it, as for any task.
func TestPayerOfAnAPITask(t *testing.T) {
	task := &Task{Spec: TaskSpec{Source: Source{Kind: "api", RequestedBy: "system:slack-app", Sponsor: "github:smana"}}}
	if got := task.Payer(); got != "system:slack-app" {
		t.Fatalf("payer %q", got)
	}
	if got := task.Sponsored(); got != "system:slack-app" {
		t.Fatalf("sponsored %q", got)
	}
	task.Status.RetriedBy = "github:alice"
	if got := task.Payer(); got != "github:alice" {
		t.Fatalf("after a retry %q", got)
	}
	issue := &Task{Spec: TaskSpec{Source: Source{Kind: "issue", RequestedBy: "github:Bob"}}}
	if issue.Payer() != "github:bob" || issue.Sponsored() != "" {
		t.Fatal("an issue's payer is unchanged")
	}
}

// L15: Renovate's adopted pull request is no person's: it is charged to nobody, even retried.
func TestPayerOfAnAdoptedTask(t *testing.T) {
	task := &Task{Spec: TaskSpec{Source: Source{Kind: "adopt", RequestedBy: "github:renovate[bot]"}}}
	if got := task.Payer(); got != "" {
		t.Fatalf("payer %q", got)
	}
	task.Status.RetriedBy = "github:alice"
	if got := task.Payer(); got != "" {
		t.Fatalf("after a retry %q", got)
	}
}
