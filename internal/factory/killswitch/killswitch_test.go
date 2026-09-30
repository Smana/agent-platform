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
