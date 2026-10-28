// SPDX-License-Identifier: Apache-2.0

package finding

import "testing"

// Both intakes write these byte for byte: a change here is a change to the retiring intake too.
func TestText(t *testing.T) {
	got := Text("RunLore: KubePodCrashLooping on apps/web", "action_required", 0.9, "apps/web", "KubePodCrashLooping", "critical", "gcp-0", "OOM")
	want := "# RunLore: KubePodCrashLooping on apps/web\n\nRunLore verdict action_required (confidence 0.90) on apps/web, " +
		"alert KubePodCrashLooping, severity critical, cluster gcp-0.\n\nOOM"
	if got != want {
		t.Fatalf("%q", got)
	}
}

func TestStubIssue(t *testing.T) {
	title, body := StubIssue("Kube<b>Pod", "apps/web [x](y)", "crit`ical", "action_required", 0.9, "abcdefgh", "https://rooms.example/")
	if title != "RunLore: Kube_b_Pod on apps/web__x__y_" {
		t.Fatalf("%q", title)
	}
	want := "RunLore reported **crit_ical** on `apps/web__x__y_` (alert `Kube_b_Pod`, verdict `action_required`, confidence 0.90).\n\n" +
		"Agent factory task `abcdefgh` investigates it. The finding and the work stay in its room: https://rooms.example/r/abcdefgh (tailnet only).\n\n" +
		"Apply `factory/stop` to stop it."
	if body != want {
		t.Fatalf("%q", body)
	}
}

func TestLabelSafe(t *testing.T) {
	if got := LabelSafe("a b!\u202e[c](d)"); got != "a_b___c__d_" {
		t.Fatalf("%q", got)
	}
}
