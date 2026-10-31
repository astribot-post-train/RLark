package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// NativeObjectReference pins an extension record to one exact RLark object incarnation.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="referenced object instance is immutable"
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
	Cluster NativeObjectReference `json:"cluster"`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="runtime is immutable"
	Runtime Runtime `json:"runtime"`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="authoritative namespace is immutable"
	AgentNamespace     string `json:"agentNamespace"`
	TargetAgentRelease string `json:"targetAgentRelease,omitempty"`
	MaxUnavailable     int32  `json:"maxUnavailable,omitempty"`
}
type ContainerClusterStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Phase              string             `json:"phase,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	// LastAgentUpgrade is the latest verified successful rollout receipt, not
	// an unbounded operation history. Only the rollout controller writes it.
	LastAgentUpgrade *AgentRolloutReceipt `json:"lastAgentUpgrade,omitempty"`
}

// AgentRolloutReceipt preserves exact completion identity before the automatic
// rollout controller deletes its successful operation to free the Cluster slot.
type AgentRolloutReceipt struct {
	Operation   ObservedObjectReference `json:"operation"`
	NodeRecord  ObservedObjectReference `json:"nodeRecord"`
	Release     ObservedObjectReference `json:"release"`
	SHA256      string                  `json:"sha256"`
	CompletedAt metav1.Time             `json:"completedAt"`
}

// ObservedObjectReference identifies an observed instance in replaceable Status
// snapshots. It intentionally has no immutable-Spec transition validation.
type ObservedObjectReference struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
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
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// AgentCredentialID is an enrollment identity, never a secret or token.
	AgentCredentialID  string             `json:"agentCredentialID,omitempty"`
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
// +kubebuilder:validation:XValidation:rule="has(self.upgrade) ? (!has(self.job) && !has(self.task) && !has(self.executionID) && !has(self.jobBindingUID) && !has(self.generation) && !has(self.jobGeneration) && !has(self.group)) : (has(self.job) && has(self.task) && has(self.executionID) && has(self.jobBindingUID) && has(self.generation) && has(self.jobGeneration))",message="allocation must identify either an upgrade operation or a workload Task"
type ContainerAllocationSpec struct {
	Cluster NativeObjectReference `json:"cluster"`
	// Upgrade is absent for legacy and canonical workload allocations.
	Upgrade *UpgradeAllocation `json:"upgrade,omitempty"`
	// +optional
	Job NativeObjectReference `json:"job,omitzero"`
	// +optional
	Task           NativeObjectReference `json:"task,omitzero"`
	Node           NativeObjectReference `json:"node"`
	TenantID       string                `json:"tenantID"`
	Generation     int64                 `json:"generation,omitempty"`
	JobGeneration  int64                 `json:"jobGeneration,omitempty"`
	ExecutionID    string                `json:"executionID,omitempty"`
	NodeInstanceID string                `json:"nodeInstanceID"`
	PrivateAddress string                `json:"privateAddress"`
	NetworkScope   string                `json:"networkScope"`
	NodeRecordUID  string                `json:"nodeRecordUID"`
	JobBindingUID  string                `json:"jobBindingUID,omitempty"`
	GPUUUIDs       []string              `json:"gpuUUIDs,omitempty"`
	// Group is immutable execution material for a Docker Ray gang. It is absent
	// for single-container and Raw allocations. The controller owns this field;
	// Agents only consume it after the exact Task and allocation identities pass
	// authorization.
	Group *AllocationGroup `json:"group,omitempty"`
}

// AllocationGroup binds one member to a durable, ordered Ray execution. The
// member list is repeated in every allocation so a lost response can be
// recovered without relying on controller memory.
type AllocationGroup struct {
	// +kubebuilder:validation:Pattern="^[a-f0-9]{64}$"
	ID string `json:"id"`
	// +kubebuilder:validation:Pattern="^[a-f0-9]{64}$"
	Digest string `json:"digest"`
	// +kubebuilder:validation:Minimum=0
	MemberIndex int32        `json:"memberIndex"`
	Plan        RayGroupPlan `json:"plan"`
}

// GroupMember is the trusted placement and rank for one group member.
type GroupMember struct {
	Task NativeObjectReference `json:"task"`
	// +kubebuilder:validation:Minimum=1
	Generation   int64                 `json:"generation"`
	Node         NativeObjectReference `json:"node"`
	NodeRecord   NativeObjectReference `json:"nodeRecord"`
	InstanceID   string                `json:"instanceID"`
	CredentialID string                `json:"credentialID"`
	PrivateAddr  string                `json:"privateAddress"`
	// +kubebuilder:validation:Minimum=0
	Rank int32 `json:"rank"`
	// +listType=set
	GPUUUIDs []string `json:"gpuUUIDs,omitempty"`
}

// RayGroupPlan is the controller-owned complete placement decision. It is
// persisted before any member allocation is exposed; ID/digest are recomputed
// at every authorization boundary. Index zero is the only head.
type RayGroupPlan struct {
	Binding NativeObjectReference `json:"binding"`
	Job     NativeObjectReference `json:"job"`
	// +kubebuilder:validation:Minimum=1
	JobGeneration int64                 `json:"jobGeneration"`
	Restart       string                `json:"restart,omitempty"`
	Cluster       NativeObjectReference `json:"cluster"`
	TenantID      string                `json:"tenantID"`
	NetworkScope  string                `json:"networkScope"`
	Members       []GroupMember         `json:"members"`
	Ports         RayPortPlan           `json:"ports"`
}

// RayPortPlan is an immutable host port block shared by all group members.
type RayPortPlan struct {
	// +kubebuilder:validation:Minimum=1024
	// +kubebuilder:validation:Maximum=65535
	Base int32 `json:"base"`
	// +kubebuilder:validation:Minimum=1
	WorkerCount int32 `json:"workerCount"`
}

// UpgradeAllocation pins a shared node lock to one exact upgrade operation.
type UpgradeAllocation struct {
	Operation NativeObjectReference `json:"operation"`
	Release   NativeObjectReference `json:"release"`
	// +kubebuilder:validation:Pattern="^[a-f0-9]{64}$"
	SHA256 string `json:"sha256"`
}
type ContainerAllocationStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Phase              string             `json:"phase,omitempty"`
	CleanupConfirmed   bool               `json:"cleanupConfirmed,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	// Prepared is written only by the certificate-bound owning Agent. It proves
	// a journalled stopped container; it is never itself a start grant.
	Prepared *ContainerPreparedReceipt `json:"prepared,omitempty"`
}

// ContainerPreparedReceipt pins the local creation journal to one allocation
// and group manifest without publishing workload configuration.
type ContainerPreparedReceipt struct {
	GroupID       string `json:"groupID"`
	GroupDigest   string `json:"groupDigest"`
	AllocationUID string `json:"allocationUID"`
	TaskUID       string `json:"taskUID"`
	Generation    int64  `json:"generation"`
	EngineID      string `json:"engineID"`
	ContainerID   string `json:"containerID"`
	// +kubebuilder:validation:Pattern="^[a-f0-9]{64}$"
	ManifestDigest string `json:"manifestDigest"`
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
	Group              *JobGroupStatus    `json:"group,omitempty"`
}

// JobGroupStatus reports durable group barriers without embedding logs or
// unbounded member history.
type JobGroupStatus struct {
	ID     string       `json:"id"`
	Digest string       `json:"digest"`
	Plan   RayGroupPlan `json:"plan"`
	// +kubebuilder:validation:Enum=Allocating;Preparing;Starting;Stopping;Succeeded;Failed
	Phase string `json:"phase"`
	// Result is retained before cleanup fan-out, including partial reservations.
	// +kubebuilder:validation:Enum=Succeeded;Failed
	Result     string             `json:"result,omitempty"`
	Reason     string             `json:"reason,omitempty"`
	StartedAt  metav1.Time        `json:"startedAt"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
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

// AgentUpgradeSpec is the immutable intent for one node upgrade. Its object
// UID is the operation-instance identity; its deterministic name serializes
// all operations for one native Cluster.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="upgrade operation identity is immutable"
type AgentUpgradeSpec struct {
	Cluster       NativeObjectReference `json:"cluster"`
	ClusterRecord NativeObjectReference `json:"clusterRecord"`
	NodeRecord    NativeObjectReference `json:"nodeRecord"`
	Release       NativeObjectReference `json:"release"`
	ReleaseSpec   AgentReleaseSpec      `json:"releaseSpec"`
	TenantID      string                `json:"tenantID"`
	InstanceID    string                `json:"instanceID"`
	CredentialID  string                `json:"credentialID"`
	FromVersion   string                `json:"fromVersion"`
}

// AgentUpgradeStatus separates reservation, application and terminal proof.
type AgentUpgradeStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +kubebuilder:validation:Enum=Pending;Reserved;Applying;Succeeded;RolledBack;Failed;Cancelled
	Phase      string                  `json:"phase,omitempty"`
	Allocation *NativeObjectReference  `json:"allocation,omitempty"`
	StartedAt  *metav1.Time            `json:"startedAt,omitempty"`
	AttemptID  string                  `json:"attemptID,omitempty"`
	Completion *AgentUpgradeCompletion `json:"completion,omitempty"`
	// AcknowledgedAttemptID is set only after the owning updater has persisted
	// its terminal journal. Automatic successful-operation reaping requires it.
	AcknowledgedAttemptID string             `json:"acknowledgedAttemptID,omitempty"`
	Conditions            []metav1.Condition `json:"conditions,omitempty"`
}

// AgentUpgradeCompletion is a bounded receipt from the node-scoped updater.
// The controller still owns allocation cleanup and terminal observation.
type AgentUpgradeCompletion struct {
	AttemptID string `json:"attemptID"`
	// +kubebuilder:validation:Enum=Succeeded;RolledBack
	Result      string      `json:"result"`
	Version     string      `json:"version"`
	HeartbeatAt metav1.Time `json:"heartbeatAt"`
	CompletedAt metav1.Time `json:"completedAt"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// AgentUpgrade is an identifiable operation and exclusive per-Cluster gate.
type AgentUpgrade struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AgentUpgradeSpec   `json:"spec"`
	Status            AgentUpgradeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// AgentUpgradeList contains AgentUpgrade objects.
type AgentUpgradeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentUpgrade `json:"items"`
}
