// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LedgerDay is the UTC day name carries after prefix, and whether name is prefix followed by a
// day at all. LegacyLedgerPrefix is a prefix of LedgerPrefix, so a USD ledger read against it
// yields "usd-<day>", which is no day: test LedgerPrefix first.
func LedgerDay(name, prefix string) (string, bool) {
	day, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return "", false
	}
	if _, err := time.Parse(LedgerDayLayout, day); err != nil {
		return "", false
	}
	return day, true
}

// SettledColumn reads a day ledger's settled.<runId> entries: the micro-USD charged to each run
// that day. Nil when there are none.
func SettledColumn(cm *corev1.ConfigMap) (map[string]int64, error) {
	var m map[string]int64
	for k, raw := range cm.Data {
		id, ok := strings.CutPrefix(k, LedgerSettled)
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("ledger %s: settled %s is %q, not a micro-USD amount", cm.Name, id, raw)
		}
		if m == nil {
			m = map[string]int64{}
		}
		m[id] = n
	}
	return m, nil
}

// AddSettled adds each run's micro-USD to its settled.<runId> entry: the meter's half of the one
// write that also carries the spent column.
func AddSettled(cm *corev1.ConfigMap, inc map[string]int64) error {
	have, err := SettledColumn(cm)
	if err != nil {
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	for id, n := range inc {
		cm.Data[LedgerSettled+id] = strconv.FormatInt(have[id]+n, 10)
	}
	return nil
}

// SettledByRun sums settled.<runId> over every day ledger in ns: what the meters have charged each
// run so far, the floor a meter that first sees a run starts it from (TC6). The pruner bounds the
// sum to the retained days. c must list ConfigMaps live: the factory's cache sees the stop object
// only. One entry that does not parse fails the whole read: a meter must not guess what was charged.
func SettledByRun(ctx context.Context, c client.Reader, ns string) (map[string]int64, error) {
	var cms corev1.ConfigMapList
	if err := c.List(ctx, &cms, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("ledger: list the day ledgers: %w", err)
	}
	out := map[string]int64{}
	for i := range cms.Items {
		if _, ok := LedgerDay(cms.Items[i].Name, LedgerPrefix); !ok {
			continue
		}
		day, err := SettledColumn(&cms.Items[i])
		if err != nil {
			return nil, err
		}
		for id, n := range day {
			out[id] += n
		}
	}
	return out, nil
}
