// SPDX-License-Identifier: Apache-2.0

package api

import (
	"maps"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func configMap(name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agent-system"}, Data: data}
}

// TC6: the ledger is one ConfigMap per UTC day, its name carrying the day after the USD prefix.
// The token ledgers it replaced share a shorter prefix, so the USD one is tested first.
func TestLedgerDay(t *testing.T) {
	for name, c := range map[string]struct {
		prefix, day string
		ok          bool
	}{
		"agent-factory-ledger-usd-20261022": {LedgerPrefix, "20261022", true},
		"agent-factory-ledger-20261022":     {LegacyLedgerPrefix, "20261022", true},
		"agent-factory-ledger-20251301":     {LegacyLedgerPrefix, "", false},
		"agent-factory-meter":               {LegacyLedgerPrefix, "", false},
	} {
		if day, ok := LedgerDay(name, c.prefix); day != c.day || ok != c.ok {
			t.Errorf("%s: %q %v, want %q %v", name, day, ok, c.day, c.ok)
		}
	}
	if _, ok := LedgerDay("agent-factory-ledger-usd-20261022", LegacyLedgerPrefix); ok {
		t.Error("a USD ledger is no token ledger")
	}
	if LedgerPrefix+"20261022" != "agent-factory-ledger-usd-20261022" {
		t.Errorf("the name is %s", LedgerPrefix+"20261022")
	}
}

// TC6: settled.<runId> is what a run was charged that day; summed over every USD day ledger it is
// all a run was charged. A token ledger, another ConfigMap and a name that is no day add nothing.
func TestSettledByRunSumsTheUSDDayLedgers(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(
		configMap(LedgerPrefix+"20260926", map[string]string{LedgerSettled + "aaaaaaaa": "1200000",
			LedgerSpent: `{"system:factory":1200000}`}),
		configMap(LedgerPrefix+"20260927", map[string]string{LedgerSettled + "aaaaaaaa": "300000",
			LedgerSettled + "bbbbbbbb": "7", LedgerReserved + "cccccccc": `{"principal":"system:factory","maxMicroUSD":2550000}`}),
		configMap(LegacyLedgerPrefix+"20260927", map[string]string{LedgerSettled + "aaaaaaaa": "999"}),
		configMap(MeterHeartbeat, map[string]string{LedgerSettled + "aaaaaaaa": "999"}),
		configMap(LedgerPrefix+"notaday", map[string]string{LedgerSettled + "aaaaaaaa": "999"}),
	).Build()
	got, err := SettledByRun(t.Context(), c, "agent-system")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int64{"aaaaaaaa": 1_500_000, "bbbbbbbb": 7}; !maps.Equal(got, want) {
		t.Fatalf("%v, want %v", got, want)
	}
	bad := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(
		configMap(LedgerPrefix+"20260927", map[string]string{LedgerSettled + "aaaaaaaa": "1.5"})).Build()
	if _, err := SettledByRun(t.Context(), bad, "agent-system"); err == nil || !strings.Contains(err.Error(), "not a micro-USD amount") {
		t.Fatalf("an amount that does not parse fails the read: %v", err)
	}
}

// The meter adds a tick's charges to what the day already holds for each run.
func TestAddSettledAccumulates(t *testing.T) {
	cm := configMap(LedgerPrefix+"20260927", map[string]string{LedgerSettled + "aaaaaaaa": "100"})
	if err := AddSettled(cm, map[string]int64{"aaaaaaaa": 50, "bbbbbbbb": 7}); err != nil {
		t.Fatal(err)
	}
	if got, _ := SettledColumn(cm); !maps.Equal(got, map[string]int64{"aaaaaaaa": 150, "bbbbbbbb": 7}) {
		t.Fatalf("%v", got)
	}
	empty := configMap(LedgerPrefix+"20260927", nil)
	if err := AddSettled(empty, map[string]int64{"aaaaaaaa": 1}); err != nil || empty.Data[LedgerSettled+"aaaaaaaa"] != "1" {
		t.Fatalf("a new day's first entry: %v %v", err, empty.Data)
	}
}
