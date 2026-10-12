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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ConfigMap is the stop object's name, in the factory's namespace. Its content is ignored.
const ConfigMap = "agent-factory-stop"

// AnnotationSource marks a stop object the control-issue poller made, so that only it lifts it.
const AnnotationSource = "agents.ogenki.io/stop-source"

// SourceIssue is the AnnotationSource of the stop object the poller publishes.
const SourceIssue = "control-issue"

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

// PublishIssue makes the control issue's stop visible to every replica (#53). Only the leader
// polls the issue, but the run API and RunLore intake serve on every replica, and each reads the
// stop object: the poller creates it when the label is on and deletes it when the label is off,
// but only an object it made. A stop a human set by hand is never lifted by the label. The
// object outlives the leader, so a new leader's first poll reconciles it either way.
func PublishIssue(ctx context.Context, c client.Client, ns string, on bool) error {
	key := types.NamespacedName{Namespace: ns, Name: ConfigMap}
	var cm corev1.ConfigMap
	err := c.Get(ctx, key, &cm)
	switch {
	case err == nil && !on:
		if cm.Annotations[AnnotationSource] != SourceIssue {
			return nil
		}
		if err := c.Delete(ctx, &cm); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("killswitch: lift %s/%s: %w", ns, ConfigMap, err)
		}
	case apierrors.IsNotFound(err) && on:
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: ConfigMap,
			Annotations: map[string]string{AnnotationSource: SourceIssue}}}
		if err := c.Create(ctx, &cm); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("killswitch: publish %s/%s: %w", ns, ConfigMap, err)
		}
	case err != nil && !apierrors.IsNotFound(err):
		return fmt.Errorf("killswitch: read %s/%s: %w", ns, ConfigMap, err)
	}
	return nil
}
