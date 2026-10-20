// SPDX-License-Identifier: Apache-2.0

package runctl

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/Smana/agent-platform/api/v1alpha1"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var testModel = Model{BaseURL: "https://api.anthropic.com/v1", Name: "claude-sonnet-5-5", Host: "api.anthropic.com", Port: 443}

func golden(t *testing.T, name string, v any) {
	t.Helper()
	got, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(p, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("%s differs; rerun with -update and review the diff:\n%s", name, got)
	}
}

func goRun() *v1alpha1.AgentRun {
	return run("run-abcdefgh", func(r *v1alpha1.AgentRun) { r.Spec.Egress.Profiles = []v1alpha1.AgentRunEgressProfile{"golang"} })
}

func TestPodGolden(t *testing.T) {
	r := goRun()
	golden(t, "pod.golden.yaml", Pod(r, testConfig(t), testModel, "abcdefgh"))
}

func TestCNPGolden(t *testing.T) {
	r := goRun()
	golden(t, "cnp.golden.yaml", NetworkPolicy(r, testConfig(t), testModel, "abcdefgh").Object)
}

// The properties a reviewer must not lose in a golden refresh.
func TestPodInvariants(t *testing.T) {
	c := testConfig(t)
	p := Pod(run("run-abcdefgh"), c, testModel, "abcdefgh")
	if p.Name != "run-abcdefgh" || p.Spec.ServiceAccountName != "run-abcdefgh" {
		t.Fatalf("name %q sa %q", p.Name, p.Spec.ServiceAccountName)
	}
	if p.Spec.RuntimeClassName == nil || *p.Spec.RuntimeClassName != "gvisor" {
		t.Fatal("RuntimeClass must be the configured one")
	}
	if p.Spec.ActiveDeadlineSeconds == nil || *p.Spec.ActiveDeadlineSeconds != 30*60 {
		t.Fatal("deadline must be maxMinutes")
	}
	if len(p.Spec.InitContainers) != 2 || p.Spec.InitContainers[0].Name != "room-bridge" || p.Spec.InitContainers[1].Name != "room-gate" {
		t.Fatalf("init containers %v", p.Spec.InitContainers)
	}
	harness := p.Spec.Containers[0]
	for _, m := range harness.VolumeMounts {
		if m.Name == "room-token" {
			t.Fatal("only the bridge mounts the room token")
		}
	}
	env := map[string]string{}
	for _, e := range harness.Env {
		env[e.Name] = e.Value
	}
	if _, ok := env["MCP_URL"]; ok {
		t.Fatal("no MCP_URL: no room_* tools on this runtime")
	}
	if env["LLM_API_KEY_FILE"] != "/run/agent/model/api_key" || env["GIT_TOKEN_FILE"] != "/run/agent/github/token" || env["LLM_BASE_URL"] != testModel.BaseURL {
		t.Fatalf("harness env %v", env)
	}
	noRoom := Pod(run("run-abcdefgh", func(r *v1alpha1.AgentRun) { r.Spec.RoomRef = "" }), c, testModel, "abcdefgh")
	if len(noRoom.Spec.InitContainers) != 0 {
		t.Fatal("a run without a room has no bridge")
	}
	tasked := Pod(run("run-abcdefgh", func(r *v1alpha1.AgentRun) { r.Labels = map[string]string{LabelTask: "T-7"} }), c, testModel, "abcdefgh")
	var taskID string
	for _, e := range tasked.Spec.Containers[0].Env {
		if e.Name == "TASK_ID" {
			taskID = e.Value
		}
	}
	if taskID != "T-7" || tasked.Labels[LabelTask] != "T-7" {
		t.Fatalf("task label must reach the env and the labels: %q %v", taskID, tasked.Labels)
	}
	c.RuntimeClassName, c.AllowUnsandboxed = "", true
	if Pod(run("run-abcdefgh"), c, testModel, "abcdefgh").Spec.RuntimeClassName != nil {
		t.Fatal("allowUnsandboxed: no RuntimeClass")
	}
}

// An ingress section is what enables ingress default-deny in Cilium; a missing one leaves the
// pod open to every peer.
func TestCNPAlwaysDeniesIngress(t *testing.T) {
	for _, room := range []string{"", "roomroom"} {
		r := run("run-abcdefgh", func(r *v1alpha1.AgentRun) { r.Spec.RoomRef = room })
		spec := NetworkPolicy(r, testConfig(t), testModel, "abcdefgh").Object["spec"].(map[string]any)
		in, ok := spec["ingress"].([]any)
		if !ok || len(in) != 1 {
			t.Fatalf("room %q: ingress %v", room, spec["ingress"])
		}
		if _, hostOnly := in[0].(map[string]any)["fromEntities"]; hostOnly != (room != "") {
			t.Fatalf("room %q: the host rule belongs to the bridge's probe only: %v", room, in[0])
		}
	}
}
