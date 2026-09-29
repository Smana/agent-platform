// Package v1alpha1 holds the Room API (SP2 §1).
// +kubebuilder:object:generate=true
// +groupName=agents.ogenki.io
package v1alpha1

import (
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
