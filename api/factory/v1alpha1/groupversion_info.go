// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 holds the factory's Task API (SP3 §4). It shares the agents.ogenki.io
// group with the Room API but lives in its own package, so the factory's semantics stay
// outside the rooms core and can be split off without touching it.
// +kubebuilder:object:generate=true
// +groupName=agents.ogenki.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// SchemeBuilder uses the apimachinery builder directly: controller-runtime's
// pkg/scheme.Builder is deprecated for api packages (SA1019, lint budget).
var (
	GroupVersion  = schema.GroupVersion{Group: "agents.ogenki.io", Version: "v1alpha1"}
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion, &Task{}, &TaskList{})
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
