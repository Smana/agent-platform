// SPDX-License-Identifier: Apache-2.0

package roomctrl

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Smana/agent-platform/internal/runwatch"
	"github.com/Smana/agent-platform/internal/store"
)

const (
	stallAfter       = 30 * time.Minute // RoomStalled (§9)
	defaultRetention = 90 * 24 * time.Hour
	maxRetentionDays = 9999 // the CRD's bound, so the duration never overflows
)

// Phase projects the log and the runs into the §1 state diagram.
func Phase(st store.RoomState, runs []runwatch.Run, pending int, now time.Time) string {
	if st.Sealed || st.ClosedAt != nil {
		return "Closed"
	}
	running := false
	for _, r := range runs {
		running = running || r.Phase == "Running"
	}
	switch {
	case pending > 0, running && now.Sub(st.LastEventAt) > stallAfter:
		return "AwaitingHuman"
	case running:
		return "Active"
	case len(runs) == 0 && st.LastSeq <= 1:
		return "Open"
	default:
		return "Idle"
	}
}

// ParseRetention reads spec.retention ("<n>d", 1 to 9999). Empty means OD-17's 90 days.
func ParseRetention(s string) (time.Duration, error) {
	if s == "" {
		return defaultRetention, nil
	}
	digits, ok := strings.CutSuffix(s, "d")
	n, err := strconv.Atoi(digits)
	if !ok || err != nil || digits[0] < '0' || digits[0] > '9' || n < 1 || n > maxRetentionDays {
		return 0, fmt.Errorf("roomctrl: retention %q is not <1-%d>d", s, maxRetentionDays)
	}
	return time.Duration(n) * 24 * time.Hour, nil
}

// clampInt32 narrows a count for a status field without wrapping (G115).
func clampInt32(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	if n < math.MinInt32 {
		return math.MinInt32
	}
	return int32(n)
}
