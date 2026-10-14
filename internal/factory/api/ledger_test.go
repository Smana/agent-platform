// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/agent-platform/internal/factory/runs"
)

func ledgerWith(reserved map[string]string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: LedgerPrefix + "20260927", Namespace: "agent-system"},
		Data: map[string]string{}}
	for id, v := range reserved {
		cm.Data[LedgerReserved+id] = v
	}
	return cm
}

func readLedger(t *testing.T, c client.Client) *corev1.ConfigMap {
	t.Helper()
	var cm corev1.ConfigMap
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "agent-system", Name: LedgerPrefix + "20260927"}, &cm); err != nil {
		t.Fatal(err)
	}
	return &cm
}

// #55: Release drops exactly the named runs' reservations, once. A second release — a replay
// after a crash, or the meter's sweep after the reconciler's — finds nothing and writes nothing,
// and another run's reservation in the same room is never touched.
func TestReleaseIsIdempotentByRunID(t *testing.T) {
	entry := `{"principal":"human:291","room":"3kq7x2ma","maxTokens":2000000}`
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(
		ledgerWith(map[string]string{"aaaaaaaa": entry, "bbbbbbbb": `{"principal":"system:factory","maxTokens":1500000}`})).Build()
	if err := Release(t.Context(), c, "agent-system", now, "aaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	cm := readLedger(t, c)
	if _, ok := cm.Data[LedgerReserved+"aaaaaaaa"]; ok {
		t.Fatal("released")
	}
	if _, ok := cm.Data[LedgerReserved+"bbbbbbbb"]; !ok {
		t.Fatal("another run's reservation is kept")
	}
	rv := cm.ResourceVersion
	for range 2 {
		if err := Release(t.Context(), c, "agent-system", now, "aaaaaaaa"); err != nil {
			t.Fatal(err)
		}
	}
	if readLedger(t, c).ResourceVersion != rv {
		t.Fatal("a second release writes nothing")
	}
	// The room is free again the same day (#55's acceptance).
	if _, err := Reserve(t.Context(), c, "agent-system", now, "cccccccc",
		Reservation{Principal: "human:291", Room: "3kq7x2ma", MaxTokens: 2_000_000}, 5_000_000, false); err != nil {
		t.Fatalf("the released room admits a new run: %v", err)
	}
	// No ledger that day: nothing to release.
	if err := Release(t.Context(), c, "agent-system", now.Add(-48*time.Hour), "aaaaaaaa"); err != nil {
		t.Fatal(err)
	}
}

// #54: admission under an enforced daily cap needs a meter that sees. No heartbeat, or one past
// its own blind interval, refuses; a fresh one admits; in shadow nothing is read and nothing held.
func TestABlindMeterRefusesEnforcedAdmission(t *testing.T) {
	mine := Reservation{Principal: runs.PrincipalFactory, MaxTokens: 1_500_000}
	for name, c := range map[string]struct {
		stamp   time.Time // zero: the meter never stamped
		enforce bool
		blind   bool
	}{
		"never metered, enforced": {enforce: true, blind: true},
		"stale, enforced":         {stamp: now.Add(-6 * time.Minute), enforce: true, blind: true},
		"fresh, enforced":         {stamp: now.Add(-4 * time.Minute), enforce: true},
		"never metered, shadow":   {},
		"stale, shadow":           {stamp: now.Add(-time.Hour)},
	} {
		t.Run(name, func(t *testing.T) {
			k := fake.NewClientBuilder().WithScheme(scheme()).Build()
			if !c.stamp.IsZero() {
				if err := StampMeter(t.Context(), k, "agent-system", c.stamp, 5*time.Minute); err != nil {
					t.Fatal(err)
				}
			}
			_, err := Reserve(t.Context(), k, "agent-system", now, "aaaaaaaa", mine, 25_000_000, c.enforce)
			if errors.Is(err, ErrMeterBlind) != c.blind || (!c.blind && err != nil) {
				t.Fatalf("want blind %v: %v", c.blind, err)
			}
		})
	}
}

// #54: the API says why it refuses: the meter, not the ledger.
func TestTheAPIRefusesWhileTheMeterIsBlind(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme()).Build()
	st := &store{}
	h := mkServer(t, st, c, true, false, "7f3cq2xz", 5_000_000)
	if err := StampMeter(t.Context(), c, "agent-system", now.Add(-time.Hour), 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if w := post(h.Handler(), "alice", base()); w.Code != http.StatusServiceUnavailable ||
		!strings.Contains(w.Body.String(), "meter_blind") || len(st.created) != 0 {
		t.Fatalf("%d %s %d", w.Code, w.Body, len(st.created))
	}
	if err := StampMeter(t.Context(), c, "agent-system", now, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if w := post(h.Handler(), "alice", base()); w.Code != http.StatusCreated {
		t.Fatalf("the meter sees again: %d %s", w.Code, w.Body)
	}
}
