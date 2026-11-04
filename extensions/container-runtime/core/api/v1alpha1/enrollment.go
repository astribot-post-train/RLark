package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// EnrollmentNode describes an SSH target. Workers are reached by running SSH on
// the gateway, using its local private key, never by exporting that key.
type EnrollmentNode struct {
	Name string `json:"name"`
	// +kubebuilder:validation:Enum=Gateway;Worker
	Role NodeRole `json:"role"`
	// Address is the SSH endpoint, which may differ from the container's IP.
	Address string `json:"address"`
	// PrivateAddress optionally pins the address discovered inside the Notebook.
	PrivateAddress string `json:"privateAddress,omitempty"`
	// +kubebuilder:default=22
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
	// NodeUID is required to start an existing node; empty means create only.
	NodeUID string `json:"nodeUID,omitempty"`
}

// NodeEnrollmentSpec is a retained one-shot operation. Release identity and
// bytes are pinned before effects; changing latest later cannot change a retry.
// +kubebuilder:validation:XValidation:rule="self.clusterRecord == oldSelf.clusterRecord && self.tenantID == oldSelf.tenantID && self.operationID == oldSelf.operationID && self.release == oldSelf.release && self.releaseSpec == oldSelf.releaseSpec && self.nodes == oldSelf.nodes",message="enrollment targets and release are immutable"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.cancelRequested) || !oldSelf.cancelRequested || (has(self.cancelRequested) && self.cancelRequested)",message="enrollment cancellation cannot be reversed"
type NodeEnrollmentSpec struct {
	// CancelRequested stops future bootstrap effects without stopping joined nodes.
	CancelRequested bool                  `json:"cancelRequested,omitempty"`
	ClusterRecord   NativeObjectReference `json:"clusterRecord"`
	TenantID        string                `json:"tenantID"`
	OperationID     string                `json:"operationID"`
	Release         NativeObjectReference `json:"release"`
	ReleaseSpec     AgentReleaseSpec      `json:"releaseSpec"`
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	Nodes []EnrollmentNode `json:"nodes"`
}

// EnrollmentNodeStatus is one bounded receipt, separate from Agent liveness.
type EnrollmentNodeStatus struct {
	Name           string `json:"name"`
	InstanceID     string `json:"instanceID,omitempty"`
	PrivateAddress string `json:"privateAddress,omitempty"`
	NodeUID        string `json:"nodeUID,omitempty"`
	// +kubebuilder:validation:Enum=Unknown;Reachable;Unreachable
	Reachability string       `json:"reachability,omitempty"`
	ProbedAt     *metav1.Time `json:"probedAt,omitempty"`
	// Services are observed by the local node manager; a live PID is not readiness.
	Services []string `json:"services,omitempty"`
	Phase    string   `json:"phase,omitempty"`
	Reason   string   `json:"reason,omitempty"`
}

type NodeEnrollmentStatus struct {
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	Phase              string `json:"phase,omitempty"`
	// +listType=map
	// +listMapKey=name
	Nodes      []EnrollmentNodeStatus `json:"nodes,omitempty"`
	Conditions []metav1.Condition     `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// NodeEnrollment records an administrator's recoverable start/join operation.
type NodeEnrollment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              NodeEnrollmentSpec   `json:"spec"`
	Status            NodeEnrollmentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// NodeEnrollmentList contains NodeEnrollment objects.
type NodeEnrollmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeEnrollment `json:"items"`
}
