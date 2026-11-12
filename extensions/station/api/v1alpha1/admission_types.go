package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodeAdmission lets one machine that is not yet a node join: an operator
// writes it and hands the machine the one-time code whose hash it holds. The
// machine redeems the code at Station API with its WireGuard public key and
// receives its tunnel configuration and what it needs to join the cluster.
// The admission stays afterwards as the node's network declaration. Its name
// is the Node name in the namespace mapped to the cluster.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=admit
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.status.address`
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.spec.expiresAt`
type NodeAdmission struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              NodeAdmissionSpec   `json:"spec"`
	Status            NodeAdmissionStatus `json:"status,omitempty"`
}

type NodeAdmissionSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	ClusterID string `json:"clusterID"`
	// CodeHash is the SHA-256 of the one-time code in hex. The code itself is
	// shown once to the operator and stored nowhere.
	// +kubebuilder:validation:MinLength=64
	// +kubebuilder:validation:MaxLength=64
	CodeHash string `json:"codeHash"`
	// ExpiresAt ends the time in which the code can be redeemed.
	ExpiresAt metav1.Time `json:"expiresAt"`
	// APIServer is the host:port of the cluster's API the machine joins at;
	// it must be reachable from the machine without the tunnel.
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=253
	APIServer string `json:"apiServer"`
	// Labels and Taints (key=value:effect) are what the node registers with.
	// +kubebuilder:validation:MaxProperties=32
	Labels map[string]string `json:"labels,omitempty"`
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=317
	Taints []string `json:"taints,omitempty"`
	// Address pins the tunnel address; empty lets the controller allocate one.
	// +kubebuilder:validation:MaxLength=15
	Address string `json:"address,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	ListenPort int32              `json:"listenPort,omitempty"`
	Uplink     *SiteNetworkUplink `json:"uplink,omitempty"`
	// PublicKey is written by Station API when the code is redeemed; the code
	// then stays valid for this key only.
	// +kubebuilder:validation:MaxLength=44
	PublicKey string `json:"publicKey,omitempty"`
	// RedeemedAt is when Station API accepted the code.
	RedeemedAt *metav1.Time `json:"redeemedAt,omitempty"`
}

// +kubebuilder:validation:Enum=Pending;Redeemed;Joined;Expired
type NodeAdmissionPhase string

const (
	// Pending: the code has not been redeemed.
	NodeAdmissionPending NodeAdmissionPhase = "Pending"
	// Redeemed: the machine has its tunnel configuration and is joining.
	NodeAdmissionRedeemed NodeAdmissionPhase = "Redeemed"
	// Joined: the node has enrolled under its own identity.
	NodeAdmissionJoined NodeAdmissionPhase = "Joined"
	// Expired: the code was not redeemed in time.
	NodeAdmissionExpired NodeAdmissionPhase = "Expired"
)

type NodeAdmissionStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Phase              NodeAdmissionPhase `json:"phase,omitempty"`
	// +kubebuilder:validation:MaxLength=512
	Reason string `json:"reason,omitempty"`
	// Address is the tunnel address; once set it stays with this admission.
	Address string `json:"address,omitempty"`
	// Config is the document station-gateway reconciles until the node has
	// enrolled under its own identity, for PublicKey.
	// +kubebuilder:validation:MaxLength=262144
	Config     string `json:"config,omitempty"`
	ConfigHash string `json:"configHash,omitempty"`
	PublicKey  string `json:"publicKey,omitempty"`
}

// +kubebuilder:object:root=true
type NodeAdmissionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeAdmission `json:"items"`
}
