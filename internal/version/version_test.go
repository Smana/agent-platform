// SPDX-License-Identifier: Apache-2.0

package version

import "testing"

func TestVersionDefaultsToDev(t *testing.T) {
	if Version != "dev" {
		t.Fatalf("an unstamped build must say dev, got %q", Version)
	}
}
