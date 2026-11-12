package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// NodeOperation records a single administrator-requested node lifecycle operation.
// Credentials are held in a separate, short-lived Secret, never in this object.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=`.spec.action`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type NodeOperation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              NodeOperationSpec   `json:"spec"`
	Status            NodeOperationStatus `json:"status,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="operation spec is immutable"
// +kubebuilder:validation:XValidation:rule="self.action == 'Join' || has(self.nodeUID)",message="existing node operations require node UID"
// +kubebuilder:validation:XValidation:rule="self.action == 'EnableWireGuard' || (has(self.ssh) && has(self.credentialSecret))",message="join and removal require SSH connection and credential Secret"
// +kubebuilder:validation:XValidation:rule="!has(self.resetExistingKubeadm) || !self.resetExistingKubeadm || self.action == 'Join'",message="stale kubeadm reset is only valid for Join"
// +kubebuilder:validation:XValidation:rule="!has(self.containerdRuntime) || self.action == 'Join'",message="containerdRuntime is only valid for Join"
type NodeOperationSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	RequestID string `json:"requestID"`
	// +kubebuilder:validation:Enum=Join;Remove;EnableWireGuard
	Action string `json:"action"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	ClusterID string `json:"clusterID"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	NodeName string `json:"nodeName"`
	// +kubebuilder:validation:MaxLength=128
	NodeUID string `json:"nodeUID,omitempty"`
	// ResetExistingKubeadm explicitly permits a Join operation to reset stale
	// kubeadm state on a verified non-control-plane host before joining it.
	ResetExistingKubeadm bool `json:"resetExistingKubeadm,omitempty"`
	// ContainerdRuntime selects one of the node-agent runtime profiles installed in the member cluster.
	// +kubebuilder:validation:Enum=containerd;fleet
	ContainerdRuntime string             `json:"containerdRuntime,omitempty"`
	SSH               *NodeSSHConnection `json:"ssh,omitempty"`
	// CredentialSecret names a Secret in the dedicated credentials namespace; the
	// controller verifies its operation UID binding before reading or deleting it.
	// +kubebuilder:validation:MaxLength=253
	CredentialSecret string `json:"credentialSecret,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	RequestedBy string `json:"requestedBy"`
}

type NodeSSHConnection struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Address string `json:"address"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	User string `json:"user"`
	// HostKeyFingerprint is the administrator-confirmed SHA256 fingerprint.
	// +kubebuilder:validation:Pattern=`^SHA256:[A-Za-z0-9+/]{43}$`
	HostKeyFingerprint string `json:"hostKeyFingerprint"`
}

type NodeOperationStatus struct {
	// +kubebuilder:validation:Enum=Pending;Running;Blocked;Succeeded;Failed
	Phase string `json:"phase,omitempty"`
	// Stage names the last verified step, not an instruction to replay an effect.
	// +kubebuilder:validation:MaxLength=128
	Stage string `json:"stage,omitempty"`
	// NodeUID identifies the actual member Node instance established by a join.
	// +kubebuilder:validation:MaxLength=128
	NodeUID            string       `json:"nodeUID,omitempty"`
	ObservedGeneration int64        `json:"observedGeneration,omitempty"`
	CompletedAt        *metav1.Time `json:"completedAt,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type NodeOperationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeOperation `json:"items"`
}
