// SPDX-License-Identifier: Apache-2.0

// Package taskid derives Task names from idempotency keys (§1, §4): the first 8 characters
// of the lowercase, unpadded RFC 4648 base32 of sha256(key), so [a-z2-7]{8} as C2 requires.
// The names are the dedup, so a key's format never changes between releases.
package taskid

import (
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"strings"
	"time"
)

// Name is the Task name for an idempotency key.
func Name(key string) string {
	sum := sha256.Sum256([]byte(key))
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:]))[:8]
}

// IssueKey is an issue task's key; gen counts maintainers' factory/ready labels on the issue (R4).
func IssueKey(repo string, number, gen int) string {
	return fmt.Sprintf("github:issue:%s#%d:gen%d", repo, number, gen)
}

// RunloreKey dedups "while a task for it is open" (§1): the caller checks openness.
func RunloreKey(alert, resourceRef string) string {
	return fmt.Sprintf("runlore:%s:%s", alert, resourceRef)
}

// ScheduleKey is a scheduled task's key: one per schedule and firing time, in UTC.
func ScheduleKey(name string, at time.Time) string {
	return fmt.Sprintf("schedule:%s:%s", name, at.UTC().Format(time.RFC3339))
}
