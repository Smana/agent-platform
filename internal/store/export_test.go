// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"

	"github.com/Smana/agent-platform/internal/envelope"
)

// Exported to package store_test only, where the fan-out hub runs against a real
// PostgreSQL: fanout imports store, so that test cannot live in package store.

// RoomForTest is the room OpenForTest creates.
const RoomForTest = room

// OpenForTest opens a store on a fresh database with RoomForTest created, and
// returns the superuser URL.
func OpenForTest(t *testing.T) (*Store, string) {
	t.Helper()
	s, _, _, super := open(t)
	return s, super
}

// DraftForTest is an agent message in RoomForTest, idempotent on (client, n).
func DraftForTest(client string, n int64) envelope.Draft { return draft(client, n) }

// TerminateListener kills the LISTEN backend, as a failover does.
func TerminateListener(t *testing.T, super string) { t.Helper(); terminateListener(t, super) }
