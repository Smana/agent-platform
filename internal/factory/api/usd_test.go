// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TC7: a run request caps in tokens until F4 brings max_cost, so it reserves its maxTokens at the
// reference rate, 1.7 micro-USD a token.
func TestARunRequestReservesItsMaxTokensAtTheReferenceRate(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme()).Build()
	s := mkServer(t, &store{}, c, false, false, "7f3cq2xz", 8_500_000)
	body := base()
	body["maxTokens"] = 1_000_000
	if w := post(s.Handler(), "alice", body); w.Code != http.StatusCreated {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if r := entry(t, s, "7f3cq2xz"); r != (Reservation{Principal: "human:291", MaxMicroUSD: 1_700_000}) {
		t.Fatalf("%+v, want 1.7 micro-USD a token", r)
	}
}
