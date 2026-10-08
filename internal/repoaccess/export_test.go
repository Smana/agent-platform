// SPDX-License-Identifier: Apache-2.0

package repoaccess

// SetMaxEntries lowers c's cache cap, so a test can fill it.
func SetMaxEntries(c *Checker, n int) { c.maxEntries = n }

// Entries is how many answers c holds.
func Entries(c *Checker) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.cache)
}
