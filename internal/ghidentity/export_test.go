// SPDX-License-Identifier: Apache-2.0

package ghidentity

// SetMaxEntries shrinks r's cache cap so a test can reach it.
func SetMaxEntries(r *Resolver, n int) { r.maxEntries = n }

// Len is the number of cached answers.
func Len(r *Resolver) int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.cache) }
