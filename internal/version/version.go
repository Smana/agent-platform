// SPDX-License-Identifier: Apache-2.0

// Package version carries the build version, stamped by -ldflags at image build.
package version

// Version is "dev" unless the build sets it:
// -ldflags "-X github.com/Smana/agent-platform/internal/version.Version=v0.1.0".
var Version = "dev"
