// SPDX-License-Identifier: Apache-2.0

// Package killswitch is the factory's own stop layer (§6.1): a ConfigMap deliberately not in
// Git, which Flux never reverts. The other layers (Kueue, the App) do not depend on the factory.
package killswitch

import (
	"context"
	"fmt"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ConfigMap is the stop object's name, in the factory's namespace. Its content is ignored.
const ConfigMap = "agent-factory-stop"

var issueStop atomic.Bool

// SetIssue records whether the pinned control issue carries factory/stop (§6.1). The poller
// sets it every minute; the label's removal is how a human resumes.
func SetIssue(on bool) { issueStop.Store(on) }

// Engaged reports whether the stop object exists. Callers pause intake on an error too, but
// stop tasks only on a definite yes: a transient API error must not revoke every run.
func Engaged(ctx context.Context, c client.Reader, ns string) (bool, error) {
	if issueStop.Load() {
		return true, nil
	}
	var cm corev1.ConfigMap
	err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: ConfigMap}, &cm)
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	}
	return false, fmt.Errorf("killswitch: read %s/%s: %w", ns, ConfigMap, err)
}
