// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/agent-platform/internal/httpx"
	"github.com/Smana/agent-platform/internal/slackapp"
)

const slackTeam = "T01ABC"

// slackFiles writes the files a valid config names and the config itself, listeners on :0.
func slackFiles(t *testing.T, channels string) string {
	t.Helper()
	dir := t.TempDir()
	for name, v := range map[string]string{"uid": "U0BOT", "tok": "xoxb-1", "sig": "shh", "cid": "id", "csec": "secret", "pid": "123",
		"key": base64.StdEncoding.EncodeToString(make([]byte, 32))} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := fmt.Sprintf(`teamID: %s
botUserIDFile: %[2]s/uid
botTokenFile: %[2]s/tok
signingSecretFile: %[2]s/sig
webhookKeyFile: %[2]s/key
clientIDFile: %[2]s/cid
clientSecretFile: %[2]s/csec
projectIDFile: %[2]s/pid
issuerURL: https://issuer.example
brokerURL: https://room-broker.agents.svc.cluster.local
roomsURL: https://rooms.example
namespace: agents
eventsAddr: 127.0.0.1:0
webhooksAddr: 127.0.0.1:0
opsAddr: 127.0.0.1:0
%s`, slackTeam, dir, channels)
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const slackChannels = "channels:\n  C03:\n    repositories: [Smana/ai-native-ref]\n    maxCostUSD: 2\n"

func fakeCluster() (client.Client, error) { return fake.NewClientBuilder().Build(), nil }

func slackLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func httpDo(t *testing.T, method string, addr net.Addr, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+addr.String()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpx.New(5*time.Second, nil).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// The three listeners bind, the probes answer, an unsigned event is refused and counted, and
// cancelling the context drains to a nil Wait.
func TestStartSlackApp(t *testing.T) {
	cfg := slackFiles(t, slackChannels)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	run, err := startSlackApp(ctx, slackLog(), []string{"--config", cfg}, os.Getenv, fakeCluster)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []net.Addr{run.Events, run.Webhooks, run.Ops} {
		if a == nil || strings.HasSuffix(a.String(), ":0") {
			t.Fatalf("not bound: %v", a)
		}
	}
	for _, p := range []string{"/healthz", "/readyz"} {
		if st, body := httpDo(t, http.MethodGet, run.Ops, p); st != http.StatusOK {
			t.Fatalf("%s: %d %s", p, st, body)
		}
	}
	if st, _ := httpDo(t, http.MethodPost, run.Events, "/slack/events"); st != http.StatusUnauthorized {
		t.Fatalf("unsigned event: %d", st)
	}
	if _, body := httpDo(t, http.MethodGet, run.Ops, "/metrics"); !strings.Contains(body, `factory_slack_requests_total{outcome="bad_signature",route="events"} 1`) {
		t.Fatalf("metrics: %s", body)
	}
	cancel()
	done := make(chan error, 1)
	go func() { done <- run.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return")
	}
}

// An empty TeamID drops every event, so the wiring sets it from the config.
func TestSlackAppIsWiredWithTheConfigsTeam(t *testing.T) {
	cfg, err := slackapp.LoadConfig(slackFiles(t, slackChannels))
	if err != nil {
		t.Fatal(err)
	}
	a := newSlackApp(cfg, fake.NewClientBuilder().Build(), nil, slackLog())
	if a.TeamID != slackTeam || a.RoomsURL != cfg.RoomsURL || len(a.Channels) != 1 {
		t.Fatalf("%+v", a)
	}
	if a.Slack == nil || a.Broker == nil || a.Threads == nil || a.WebhookKeys == nil || a.Verifier.Secret == nil {
		t.Fatalf("unwired: %+v", a)
	}
	if id := a.BotUserID; id != "U0BOT" {
		t.Fatalf("bot user %q", id)
	}
	if keys, err := a.WebhookKeys(); err != nil || len(keys) != 1 {
		t.Fatalf("%v %v", keys, err)
	}
	if a.Verifier.Secret() != "shh" {
		t.Fatal("signing secret")
	}
}

func TestRunSlackAppRefusesABadConfig(t *testing.T) {
	cfg := slackFiles(t, "")
	err := RunSlackApp(t.Context(), slackLog(), []string{"--config", cfg}, os.Getenv)
	if err == nil || !strings.Contains(err.Error(), "config:") {
		t.Fatalf("%v", err)
	}
}

func TestStartSlackAppNeedsAConfigFlag(t *testing.T) {
	if _, err := startSlackApp(t.Context(), slackLog(), nil, func(string) string { return "" }, fakeCluster); err == nil {
		t.Fatal("no --config and no env must fail")
	}
}
