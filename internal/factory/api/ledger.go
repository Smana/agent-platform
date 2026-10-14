// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Release drops the reservations of ids from the ledger of day's UTC day (#55): a run's
// occupancy ends however the run ended, its spend stays in the spent column. Idempotent by run
// id — an absent ledger or entry is already released, and a conflict re-reads — so a replay after
// a crash, or a second releaser, drops nothing twice and never another run's entry.
func Release(ctx context.Context, c client.Client, ns string, day time.Time, ids ...string) error {
	name := LedgerPrefix + day.UTC().Format(LedgerDayLayout)
	key := types.NamespacedName{Namespace: ns, Name: name}
	for range LedgerAttempts {
		cm := &corev1.ConfigMap{}
		err := c.Get(ctx, key, cm)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("ledger %s: %w", name, err)
		}
		held := false
		for _, id := range ids {
			if _, ok := cm.Data[LedgerReserved+id]; ok {
				delete(cm.Data, LedgerReserved+id)
				held = true
			}
		}
		if !held {
			return nil
		}
		switch err := c.Update(ctx, cm); {
		case err == nil:
			return nil
		case apierrors.IsConflict(err):
			continue // an admission landed beside us: re-read and drop again
		default:
			return fmt.Errorf("ledger %s: release %v: %w", name, ids, err)
		}
	}
	return fmt.Errorf("ledger %s: %w: %v kept", name, ErrLedgerBusy, ids)
}

// The meter's heartbeat (#54). Every replica admits but only the leader meters, so the meter's
// health lives in the apiserver beside the ledgers: one ConfigMap the meter stamps after each
// tick that metered, holding when, and how long admission may go on without a newer stamp.
const (
	MeterHeartbeat   = "agent-factory-meter"
	meterLastSuccess = "lastSuccess"
	meterBlindAfter  = "blindAfter"
)

// ErrMeterBlind is Reserve's refusal while the meter has not metered within its blind interval, so no cap it feeds holds.
var ErrMeterBlind = errors.New("ledger: the meter is blind")

// StampMeter records that the meter metered at at, and the interval past which admission
// counts it blind. A failed stamp is the next tick's to retry.
func StampMeter(ctx context.Context, c client.Client, ns string, at time.Time, blindAfter time.Duration) error {
	cm := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: MeterHeartbeat}, cm)
	found := err == nil
	switch {
	case apierrors.IsNotFound(err):
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: MeterHeartbeat, Namespace: ns}}
	case err != nil:
		return fmt.Errorf("meter heartbeat: %w", err)
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[meterLastSuccess] = at.UTC().Format(time.RFC3339)
	cm.Data[meterBlindAfter] = blindAfter.String()
	if found {
		err = c.Update(ctx, cm)
	} else {
		err = c.Create(ctx, cm)
	}
	if err != nil {
		return fmt.Errorf("meter heartbeat: %w", err)
	}
	return nil
}

// meterBlind: no stamp, one that does not parse, or one older than its own interval. A meter
// that never metered is blind: an enforced cap fails closed.
func meterBlind(ctx context.Context, c client.Reader, ns string, now time.Time) (bool, error) {
	cm := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: MeterHeartbeat}, cm)
	switch {
	case apierrors.IsNotFound(err):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("meter heartbeat: %w", err)
	}
	at, err := time.Parse(time.RFC3339, cm.Data[meterLastSuccess])
	if err != nil {
		return true, nil
	}
	after, err := time.ParseDuration(cm.Data[meterBlindAfter])
	if err != nil {
		return true, nil
	}
	return now.Sub(at) > after, nil
}
