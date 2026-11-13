package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodeClass is what every node of one kind registers with: a robot model, a
// GPU server. It is declared once for the whole management plane, so that the
// hundredth machine of a kind comes in the way the first did.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
type NodeClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              NodeClassSpec `json:"spec"`
}

type NodeClassSpec struct {
	// Labels and Taints (key=value:effect) are what a node of the class
	// registers with.
	// +kubebuilder:validation:MaxProperties=32
	Labels map[string]string `json:"labels,omitempty"`
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=317
	Taints []string `json:"taints,omitempty"`
}

// +kubebuilder:object:root=true
type NodeClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeClass `json:"items"`
}

// NodeEnrollment lets a number of machines join one cluster as nodes of one
// class with a single code: an operator writes it once and runs the same
// command on every machine. Each machine that redeems the code gets a
// NodeAdmission of its own, named after the machine, and from there on joins
// exactly as a machine admitted one by one.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=enroll
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.nodeClass`
// +kubebuilder:printcolumn:name="Used",type=integer,JSONPath=`.status.uses`
// +kubebuilder:printcolumn:name="Of",type=integer,JSONPath=`.spec.maxUses`
// +kubebuilder:printcolumn:name="Expires",type=string,JSONPath=`.spec.expiresAt`
type NodeEnrollment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              NodeEnrollmentSpec   `json:"spec"`
	Status            NodeEnrollmentStatus `json:"status,omitempty"`
}

type NodeEnrollmentSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	ClusterID string `json:"clusterID"`
	// CodeHash is the SHA-256 of the code in hex. The code itself is shown
	// once to the operator and stored nowhere.
	// +kubebuilder:validation:MinLength=64
	// +kubebuilder:validation:MaxLength=64
	CodeHash string `json:"codeHash"`
	// ExpiresAt ends the time in which a new machine can redeem the code.
	ExpiresAt metav1.Time `json:"expiresAt"`
	// MaxUses is how many machines the code admits.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=500
	MaxUses int32 `json:"maxUses"`
	// NodeClass names the NodeClass every machine joins as.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	NodeClass string `json:"nodeClass"`
	// APIServer is the host:port of the cluster's API the machines join at;
	// it must be reachable from them without the tunnel.
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=253
	APIServer string `json:"apiServer"`
	// Revoked ends the code for machines that have not redeemed it yet.
	Revoked bool `json:"revoked,omitempty"`
}

type NodeEnrollmentStatus struct {
	// Uses is the number of Machines.
	Uses int32 `json:"uses,omitempty"`
	// Machines are the machines the code admitted, written by Station API.
	// +kubebuilder:validation:MaxItems=500
	Machines []EnrolledMachine `json:"machines,omitempty"`
}

type EnrolledMachine struct {
	// PublicKey is the machine's WireGuard public key; the code stays valid
	// for it, so a machine that runs the command again does not use it twice.
	// +kubebuilder:validation:MaxLength=44
	PublicKey string `json:"publicKey"`
	// NodeName is the machine's host name and the name of its NodeAdmission.
	// +kubebuilder:validation:MaxLength=63
	NodeName   string      `json:"nodeName"`
	RedeemedAt metav1.Time `json:"redeemedAt"`
}

// +kubebuilder:object:root=true
type NodeEnrollmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeEnrollment `json:"items"`
}
