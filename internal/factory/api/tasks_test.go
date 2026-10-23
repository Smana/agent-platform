// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
)

// F4, invariant 1: a sponsored token never starts a bare run, and reserves nothing on the way.
func TestRunsRefusesSponsored(t *testing.T) {
	st := &store{}
	c := fake.NewClientBuilder().WithScheme(scheme()).Build()
	s := mkServer(t, st, c, false, false, "7f3cq2xz", 5_000_000)
	s.Auth = byToken{"slack": authn.Principal{Kind: envelope.ActorSystem, ID: "system:slack-app", Sponsored: true, AccessToken: "t"}}
	w := post(s.Handler(), "slack", base())
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"error":"sponsored_only_tasks"`) || len(st.created) != 0 {
		t.Fatalf("%d %s %d", w.Code, w.Body, len(st.created))
	}
	var cms corev1.ConfigMapList
	if err := c.List(t.Context(), &cms); err != nil || len(cms.Items) != 0 {
		t.Fatalf("a ledger was written: %d %v", len(cms.Items), err)
	}
}
