// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Room is one append-only session log (SP2 §1). Rooms are runtime objects, created
// by SP3's factory or the broker, never committed to Git.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Driver",type=string,JSONPath=`.status.driver`
// +kubebuilder:printcolumn:name="Seq",type=integer,JSONPath=`.status.lastSeq`
// +kubebuilder:printcolumn:name="Pending",type=integer,JSONPath=`.status.pendingApprovals`
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.dataClass`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z2-7]{8}$')",message="a Room is named with a C2 id: 8 characters of [a-z2-7]"
type Room struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              RoomSpec   `json:"spec"`
	Status            RoomStatus `json:"status,omitempty"`
}

// RoomList is a list of Rooms.
// +kubebuilder:object:root=true
type RoomList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Room `json:"items"`
}

// RoomSpec is who owns and drives the room, who may join, and its policies.
type RoomSpec struct {
	// A principal, "human:<sub>" or "system:<name>". OIDC Core §2 caps a sub at 255
	// characters, hence 261 with the "human:" prefix, here and for every principal.
	// +kubebuilder:validation:Pattern=`^(human:[A-Za-z0-9@._-]+|system:[a-z0-9-]+)$`
	// +kubebuilder:validation:MaxLength=261
	Owner string `json:"owner"`
	// The initial driver-token holder; afterwards the log decides.
	// +kubebuilder:validation:Pattern=`^(human:[A-Za-z0-9@._-]+|system:[a-z0-9-]+)$`
	// +kubebuilder:validation:MaxLength=261
	Driver string `json:"driver"`
	// Runs join through their own spec.roomRef, never through this list.
	// +kubebuilder:validation:MaxItems=20
	// +listType=map
	// +listMapKey=principal
	// +optional
	Members []Member `json:"members,omitempty"`
	// {} so that the profile and ttl defaults apply even when approvals is omitted (review M2).
	// +kubebuilder:default={}
	// +optional
	Approvals Approvals `json:"approvals,omitempty"`
	// Fixed at creation: the log's row keeps the retention it was created with.
	// +kubebuilder:default="90d"
	// +kubebuilder:validation:Pattern=`^[1-9][0-9]{0,3}d$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="retention is immutable"
	// +optional
	Retention string `json:"retention,omitempty"`
	// Runs requested for this room inherit it (C3).
	// +kubebuilder:validation:Enum=public;internal
	DataClass string `json:"dataClass"`
	// +kubebuilder:default="Smana/cloud-native-ref"
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`
	// +optional
	Repository string `json:"repository,omitempty"`
}

// Member is a human admitted to the room, with a role and the approver flag.
type Member struct {
	// +kubebuilder:validation:Pattern=`^human:[A-Za-z0-9@._-]+$`
	// +kubebuilder:validation:MaxLength=261
	Principal string `json:"principal"`
	// Cumulative: watcher < collaborator < owner (§1).
	// +kubebuilder:validation:Enum=watcher;collaborator;owner
	Role string `json:"role"`
	// +optional
	Approver bool `json:"approver,omitempty"`
}

// Approvals is the room's approval policy (§6).
type Approvals struct {
	// +kubebuilder:default=attended
	// +kubebuilder:validation:Enum=attended;unattended
	// +optional
	Profile string `json:"profile,omitempty"`
	// Per-class override of the profile table (§6).
	// +kubebuilder:validation:XValidation:rule="self.all(k, k in ['forge.push','forge.pr','forge.other','mcp.write','shell.high'])",message="unknown approval class"
	// +kubebuilder:validation:XValidation:rule="self.all(k, self[k] in ['allow','deny','human'])",message="an override is allow, deny or human"
	// +optional
	Overrides map[string]string `json:"overrides,omitempty"`
	// +kubebuilder:default="4h"
	// At most 9999h, bounded like retention so it always fits a time.Duration.
	// +kubebuilder:validation:Pattern=`^[1-9][0-9]{0,3}(m|h)$`
	// +optional
	TTL string `json:"ttl,omitempty"`
	// OD-16: approver must differ from the humans who prompted the turn.
	// +optional
	FourEyes bool `json:"fourEyes,omitempty"`
}

// RoomStatus mirrors the log: phase, last seq, driver and pending approvals.
type RoomStatus struct {
	// +kubebuilder:validation:Enum=Open;Active;Idle;AwaitingHuman;Closed
	// +optional
	Phase            string `json:"phase,omitempty"`
	LastSeq          int64  `json:"lastSeq,omitempty"`
	Driver           string `json:"driver,omitempty"`
	DriverEpoch      int64  `json:"driverEpoch,omitempty"`
	PendingApprovals int32  `json:"pendingApprovals,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion, &Room{}, &RoomList{})
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
