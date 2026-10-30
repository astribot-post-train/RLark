package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// NativeObjectReference pins an extension record to one exact RLark object incarnation.
type NativeObjectReference struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// Runtime identifies an implemented container execution boundary.
// +kubebuilder:validation:Enum=Raw;Docker
type Runtime string

// ContainerCluster stores container-runtime settings for a native RLark Cluster.
type ContainerClusterSpec struct {
	Cluster            NativeObjectReference `json:"cluster"`
	Runtime            Runtime               `json:"runtime"`
	AgentNamespace     string                `json:"agentNamespace"`
	TargetAgentRelease string                `json:"targetAgentRelease,omitempty"`
	MaxUnavailable     int32                 `json:"maxUnavailable,omitempty"`
}
type ContainerClusterStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Phase              string             `json:"phase,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cc
// ContainerCluster is the extension-owned runtime view of a native Cluster.
type ContainerCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ContainerClusterSpec   `json:"spec,omitempty"`
	Status            ContainerClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// ContainerClusterList contains ContainerCluster objects.
type ContainerClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ContainerCluster `json:"items"`
}

// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="node identity and authorized capability are immutable; explicitly replace the node after cleanup"
type ContainerNodeSpec struct {
	Cluster    NativeObjectReference `json:"cluster"`
	Node       NativeObjectReference `json:"node"`
	Runtime    Runtime               `json:"runtime"`
	InstanceID string                `json:"instanceID"`
	// Operator-provisioned capability; Agent visibility cannot grant GPUs.
	PrivateAddress string `json:"privateAddress"`
	Environment    string `json:"environment"`
	NetworkScope   string `json:"networkScope"`
	// +listType=set
	AuthorizedGPUUUIDs []string `json:"authorizedGPUUUIDs,omitempty"`
}
type ContainerNodeStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	AgentVersion       string             `json:"agentVersion,omitempty"`
	TargetAgentVersion string             `json:"targetAgentVersion,omitempty"`
	PrivateAddress     string             `json:"privateAddress,omitempty"`
	Schedulable        bool               `json:"schedulable,omitempty"`
	Busy               bool               `json:"busy,omitempty"`
	Capacity           map[string]string  `json:"capacity,omitempty"`
	AuthorizedGPUUUIDs []string           `json:"authorizedGPUUUIDs,omitempty"`
	Capabilities       []string           `json:"capabilities,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	LastHeartbeatTime  *metav1.Time       `json:"lastHeartbeatTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// ContainerNode records extension-owned runtime identity and observations.
type ContainerNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ContainerNodeSpec   `json:"spec,omitempty"`
	Status            ContainerNodeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// ContainerNodeList contains ContainerNode objects.
type ContainerNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ContainerNode `json:"items"`
}

// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="allocation identity is immutable"
type ContainerAllocationSpec struct {
	Cluster        NativeObjectReference `json:"cluster"`
	Job            NativeObjectReference `json:"job"`
	Task           NativeObjectReference `json:"task"`
	Node           NativeObjectReference `json:"node"`
	TenantID       string                `json:"tenantID"`
	Generation     int64                 `json:"generation"`
	ExecutionID    string                `json:"executionID"`
	NodeInstanceID string                `json:"nodeInstanceID"`
	GPUUUIDs       []string              `json:"gpuUUIDs,omitempty"`
}
type ContainerAllocationStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Phase              string             `json:"phase,omitempty"`
	CleanupConfirmed   bool               `json:"cleanupConfirmed,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// ContainerAllocation is a durable exclusive reservation and cleanup record.
type ContainerAllocation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ContainerAllocationSpec   `json:"spec,omitempty"`
	Status            ContainerAllocationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// ContainerAllocationList contains ContainerAllocation objects.
type ContainerAllocationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ContainerAllocation `json:"items"`
}

// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Job controller ownership is immutable"
type ContainerJobSpec struct {
	Cluster  NativeObjectReference `json:"cluster"`
	Job      NativeObjectReference `json:"job"`
	TenantID string                `json:"tenantID"`
	Runtime  Runtime               `json:"runtime"`
}
type ContainerJobStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Phase              string             `json:"phase,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// ContainerJob is a typed ownership binding to one native Job UID.
type ContainerJob struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ContainerJobSpec   `json:"spec,omitempty"`
	Status            ContainerJobStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// ContainerJobList contains ContainerJob objects.
type ContainerJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ContainerJob `json:"items"`
}

// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="release artifacts are immutable"
type AgentReleaseSpec struct {
	Version      string `json:"version"`
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	// +kubebuilder:validation:Pattern="^[a-f0-9]{64}$"
	SHA256       string `json:"sha256"`
	ArtifactPath string `json:"artifactPath"`
	Protocol     string `json:"protocol"`
}
type AgentReleaseStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Available          bool               `json:"available,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// AgentRelease identifies an immutable host-served extension Agent artifact.
type AgentRelease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AgentReleaseSpec   `json:"spec,omitempty"`
	Status            AgentReleaseStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// AgentReleaseList contains AgentRelease objects.
type AgentReleaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentRelease `json:"items"`
}
