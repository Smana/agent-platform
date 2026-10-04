// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

func runlore(t *testing.T) (*RunLore, client.Client, *forge.Fake) {
	t.Helper()
	s := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&v1alpha1.Task{}).Build()
	f := forge.NewFake()
	return &RunLore{Forge: f, Client: c, Namespace: "agent-system", Token: func() string { return "s3cret" },
		Stopped: func(context.Context) bool { return false }, Now: func() time.Time { return time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC) },
		Errors: &counter{},
		Cfg: &config.Config{Repository: "Smana/cloud-native-ref", RoomsURL: "https://rooms.priv.aws.ogenki.io",
			RunLore: config.RunLore{MinConfidence: 0.75, DailyCap: 5}}}, c, f
}

func send(h http.Handler, tok string, fd Finding) *httptest.ResponseRecorder {
	b, _ := json.Marshal(fd)
	r := httptest.NewRequestWithContext(context.Background(), "POST", "/intake/runlore", bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

var finding = Finding{Title: "image-gallery crash-loops on a missing env var", Verdict: "action_required", Confidence: 0.82,
	AlertName: "KubePodCrashLooping", ResourceRef: "apps/xplane-image-gallery", Severity: "critical", Cluster: "aws-0",
	Text: "The pod reads S3_BUCKET, which the last release renamed. INTERNAL DETAIL 10.0.3.7"}

// SC-9: one payload replayed twice is one issue and one task.
func TestReplayYieldsOneIssueAndOneTask(t *testing.T) {
	h, c, f := runlore(t)
	if w := send(h, "s3cret", finding); w.Code != http.StatusCreated {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := send(h, "s3cret", finding); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "duplicate") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var l v1alpha1.TaskList
	_ = c.List(context.Background(), &l)
	if len(l.Items) != 1 || len(f.Created()) != 1 {
		t.Fatalf("tasks %d issues %d", len(l.Items), len(f.Created()))
	}
	tk := l.Items[0]
	if tk.Spec.Source.Kind != "runlore" || tk.Spec.DataClass != "internal" || tk.Spec.Source.Trust != "untrusted" ||
		tk.Spec.Issue == 0 || !strings.Contains(tk.Spec.Text, "S3_BUCKET") {
		t.Fatalf("%+v", tk.Spec)
	}
	issue := f.Created()[0]
	if strings.Contains(issue, "INTERNAL DETAIL") || strings.Contains(issue, "S3_BUCKET") || !strings.Contains(issue, "factory/proposed") ||
		strings.Contains(issue, "factory/ready") {
		t.Fatalf("the public issue carries no internal text and no trigger label (R18, R33): %s", issue)
	}
}

func TestBarAuthAndCap(t *testing.T) {
	h, _, _ := runlore(t)
	if w := send(h, "wrong", finding); w.Code != http.StatusUnauthorized {
		t.Fatal(w.Code)
	}
	low := finding
	low.Confidence = 0.6
	if w := send(h, "s3cret", low); w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"accepted":false`) {
		t.Fatal("below 0.75 (OD-9)")
	}
	noop := finding
	noop.Verdict = "no_action"
	if w := send(h, "s3cret", noop); w.Code != http.StatusAccepted {
		t.Fatal("not actionable")
	}
	for i := 0; i < 5; i++ {
		fd := finding
		fd.ResourceRef = "apps/r" + string(rune('a'+i))
		if w := send(h, "s3cret", fd); w.Code != http.StatusCreated {
			t.Fatalf("finding %d: %d", i, w.Code)
		}
	}
	sixth := finding
	sixth.ResourceRef = "apps/rz"
	if w := send(h, "s3cret", sixth); w.Code != http.StatusTooManyRequests {
		t.Fatalf("at most 5 a day (OD-9): %d", w.Code)
	}
}
