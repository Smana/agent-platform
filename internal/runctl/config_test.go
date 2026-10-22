// SPDX-License-Identifier: Apache-2.0

package runctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/Smana/agent-platform/api/v1alpha1"
)

const goodConfig = `
namespace: agents
namePrefix: run-
runtimeClassName: gvisor
modelSecret: agent-model
gitSecret: agent-git
broker:
  url: https://room-broker.agent-platform.svc.cluster.local:8443
  namespace: agent-platform
  labels: {app.kubernetes.io/name: room-broker}
  caSecret: room-broker-tls
images:
  harness: ghcr.io/smana/agent-harness:v0.16.0
  bridge: ghcr.io/smana/room-bridge:v0.16.0
`

func cfgFrom(t *testing.T, body string) (Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(p)
}

func testConfig(t *testing.T) Config {
	t.Helper()
	c, err := cfgFrom(t, goodConfig)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func run(name string, mut ...func(*v1alpha1.AgentRun)) *v1alpha1.AgentRun {
	r := &v1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agents", UID: "uid-1"},
		Spec: v1alpha1.AgentRunSpec{Role: "implementer", Repository: "o/r", BaseRef: "main", Principal: "human:a",
			DataClass: "public", RoomRef: "roomroom", Task: v1alpha1.AgentRunTask{Text: "fix it"},
			Budget: v1alpha1.AgentRunBudget{MaxMinutes: 30}, Size: "small"}}
	for _, m := range mut {
		m(r)
	}
	return r
}

func TestLoadConfigDefaultsTheEgressProfiles(t *testing.T) {
	c := testConfig(t)
	if got := c.EgressProfiles["github"]; len(got) != 6 || got[0] != "github.com" {
		t.Fatalf("github profile %v", got)
	}
	for _, p := range []string{"pypi", "npm", "golang", "crates"} {
		if len(c.EgressProfiles[p]) == 0 {
			t.Errorf("profile %s missing", p)
		}
	}
}

func TestLoadConfigRefuses(t *testing.T) {
	for name, body := range map[string]string{
		"no images":     strings.Replace(goodConfig, "images:", "imagesX:", 1),
		"http broker":   strings.Replace(goodConfig, "https://room-broker", "http://room-broker", 1),
		"short svc":     strings.Replace(goodConfig, ".svc.cluster.local", ".svc", 1),
		"bad prefix":    strings.Replace(goodConfig, "namePrefix: run-", "namePrefix: run", 1),
		"unknown field": goodConfig + "surprise: 1\n",
		"no model":      strings.Replace(goodConfig, "modelSecret: agent-model\n", "", 1),
	} {
		if _, err := cfgFrom(t, body); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestInvalid(t *testing.T) {
	c := testConfig(t)
	for name, tc := range map[string]struct {
		r    *v1alpha1.AgentRun
		want string
	}{
		"valid":        {run("run-abcdefgh"), ""},
		"other prefix": {run("xplane-run-abcdefgh"), "namePrefix"},
		"bad id":       {run("run-ABCDEFGH"), "namePrefix"},
		"other ns":     {run("run-abcdefgh", func(r *v1alpha1.AgentRun) { r.Namespace = "default" }), "namespace"},
		"suspendable":  {run("run-abcdefgh", func(r *v1alpha1.AgentRun) { r.Spec.Suspendable = true }), "suspendable"},
		"bogus profile": {run("run-abcdefgh", func(r *v1alpha1.AgentRun) {
			r.Spec.Egress.Profiles = []v1alpha1.AgentRunEgressProfile{"bogus"}
		}), "egress"},
		"internal": {run("run-abcdefgh", func(r *v1alpha1.AgentRun) { r.Spec.DataClass = "internal" }), "internal"},
	} {
		if got := c.Invalid(tc.r); (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: Invalid = %q, want it to mention %q", name, got, tc.want)
		}
	}
	c.RuntimeClassName = ""
	if got := c.Invalid(run("run-abcdefgh")); !strings.Contains(got, "runtimeClassName") {
		t.Errorf("no RuntimeClass: %q", got)
	}
	c.AllowUnsandboxed = true
	if got := c.Invalid(run("run-abcdefgh")); got != "" {
		t.Errorf("allowUnsandboxed: %q", got)
	}
}

func TestModelFrom(t *testing.T) {
	m, err := ModelFrom(map[string][]byte{"api_key": []byte("k"), "model": []byte("claude-sonnet-5-5")})
	if err != nil || m.BaseURL != "https://api.anthropic.com/v1" || m.Host != "api.anthropic.com" || m.Port != 443 {
		t.Fatalf("defaults: %+v %v", m, err)
	}
	m, err = ModelFrom(map[string][]byte{"api_key": []byte("k"), "model": []byte("m"), "base_url": []byte("http://stub.e2e.svc:8080/v1")})
	if err != nil || m.Host != "stub.e2e.svc" || m.Port != 8080 {
		t.Fatalf("explicit: %+v %v", m, err)
	}
	m, err = ModelFrom(map[string][]byte{"api_key": []byte("k"), "model": []byte("m"), "base_url": []byte("http://stub.e2e.svc.cluster.local:8080")})
	if err != nil || m.Host != "stub.e2e.svc.cluster.local" || m.Port != 8080 {
		t.Fatalf("cluster-local http: %+v %v", m, err)
	}
	if _, err := ModelFrom(map[string][]byte{"api_key": []byte("k"), "model": []byte("m"), "base_url": []byte("http://api.example.com/v1")}); err == nil || !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("http public host: want a base_url error, got %v", err)
	}
	if _, err := ModelFrom(map[string][]byte{"model": []byte("m")}); err == nil {
		t.Fatal("no api_key: want an error")
	}
	if _, err := ModelFrom(map[string][]byte{"api_key": []byte("k")}); err == nil {
		t.Fatal("no model: want an error")
	}
}
