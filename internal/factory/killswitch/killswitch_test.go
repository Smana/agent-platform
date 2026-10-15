// SPDX-License-Identifier: Apache-2.0

package killswitch

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestEngaged(t *testing.T) {
	c := fake.NewClientBuilder().WithObjects(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMap, Namespace: "elsewhere"}}).Build()
	if on, err := Engaged(t.Context(), c, "agent-system"); on || err != nil {
		t.Fatal("no stop object in the factory's namespace: running")
	}
	if err := c.Create(t.Context(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMap, Namespace: "agent-system"}}); err != nil {
		t.Fatal(err)
	}
	if on, err := Engaged(t.Context(), c, "agent-system"); !on || err != nil {
		t.Fatal("stop object present: engaged")
	}
}

// A transient read error is no answer: callers pause intake on it, and stop tasks only on a yes.
func TestAReadErrorIsNotAYes(t *testing.T) {
	c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("apiserver unavailable")
		}}).Build()
	if on, err := Engaged(t.Context(), c, "agent-system"); on || err == nil {
		t.Fatalf("on=%v err=%v", on, err)
	}
}

// Engaged reads only the published stop object: a PublishIssue made through another client
// leaves a reader holding no object running, and no process-local flag says otherwise.
func TestEngagedReadsOnlyThePublishedObject(t *testing.T) {
	other := fake.NewClientBuilder().Build()
	if err := PublishIssue(t.Context(), other, "agent-system", true); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().Build()
	if on, err := Engaged(t.Context(), reader, "agent-system"); on || err != nil {
		t.Fatalf("a reader without the object: on=%v err=%v", on, err)
	}
	if on, err := Engaged(t.Context(), other, "agent-system"); !on || err != nil {
		t.Fatalf("a reader with the object: on=%v err=%v", on, err)
	}
}
