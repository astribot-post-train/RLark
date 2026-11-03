// Package noderesources defines the read-only Raw Node resource projection.
// Allocation objects remain the only scheduling locks; this view grants no work.
package noderesources

import (
	"fmt"
	"slices"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/core/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/core/pkg/allocation"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/core/pkg/artifact"
	tenant "github.com/rlinf/rlark/api/metadata"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ProjectionAnnotation = "containers.rlark.io/native-resource-projection"
	ObservedAtAnnotation = "containers.rlark.io/resources-observed-at"
	ResourcesReady       = "RawResourcesReady"
	SchedulingReady      = "RawSchedulingReady"
	Freshness            = 45 * time.Second
)

// View distinguishes resource bounds, durable workload accounting and eligibility.
// A nil Used means accounting is unknown; zero quantities mean confirmed no hold.
type View struct {
	Capacity        corev1.ResourceList
	Used            corev1.ResourceList
	AccountingKnown bool
	ResourcesValid  bool
	Schedulable     bool
	Reason          string
}

func Fresh(at *metav1.Time, now time.Time) bool {
	return at != nil && !at.After(now) && now.Sub(at.Time) <= Freshness
}

func current(cs []metav1.Condition, name string, generation int64) bool {
	c := meta.FindStatusCondition(cs, name)
	return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == generation
}

// Evaluate consumes a current, identity-validated binding and an uncached lock
// inventory. Callers must not pass a filtered list that can hide conflicting locks.
// Offline observations still account for retained allocations.
func Evaluate(cn *ext.ContainerNode, nn *native.Node, cc *ext.ContainerCluster, nc *native.Cluster, locks []ext.ContainerAllocation, inventoryKnown, releaseReady bool, now time.Time) View {
	v := View{Reason: "ResourceObservationMissing"}
	capacity := corev1.ResourceList{}
	for key, value := range cn.Status.Capacity {
		if key != "cpu" && key != "memory" && key != "nvidia.com/gpu" {
			return v
		}
		q, err := resource.ParseQuantity(value)
		if err != nil || q.Sign() < 0 {
			return v
		}
		capacity[corev1.ResourceName(key)] = q
	}
	cpu := capacity[corev1.ResourceCPU]
	gpu := capacity[corev1.ResourceName("nvidia.com/gpu")]
	if cn.Status.ResourceObservedAt != nil && cpu.Sign() > 0 && gpu.Cmp(*resource.NewQuantity(int64(len(cn.Spec.AuthorizedGPUUUIDs)), resource.DecimalSI)) == 0 && sameGPUs(cn.Status.AuthorizedGPUUUIDs, cn.Spec.AuthorizedGPUUUIDs) {
		v.Capacity = capacity
	}
	if !inventoryKnown {
		v.Reason = "AllocationInventoryUnavailable"
		return v
	}
	var held *ext.ContainerAllocation
	for i := range locks {
		a := &locks[i]
		// Include malformed and foreign candidates at the canonical lock key.
		if a.Spec.Node.UID != string(nn.UID) && a.Spec.NodeRecordUID != string(cn.UID) && (a.Namespace != cn.Namespace || a.Name != allocation.Name(string(nn.UID))) {
			continue
		}
		if held != nil || a.UID == "" || a.Name != allocation.Name(string(nn.UID)) || a.Namespace != cn.Namespace || a.Spec.Cluster != cn.Spec.Cluster || a.Spec.Node != cn.Spec.Node || a.Spec.NodeRecordUID != string(cn.UID) || tenant.IDOf(a) != tenant.IDOf(cn) || a.Spec.TenantID != tenant.IDOf(cn) || a.Spec.NodeInstanceID != cn.Spec.InstanceID || a.Spec.PrivateAddress != cn.Spec.PrivateAddress || a.Spec.NetworkScope != cn.Spec.NetworkScope || !sameGPUs(a.Spec.GPUUUIDs, cn.Spec.AuthorizedGPUUUIDs) {
			v.Reason = "AllocationIdentityConflict"
			return v
		}
		held = a
	}
	if nn.Status.Reservation != nil || len(nn.Status.DockerReservations) != 0 {
		v.Reason = "LegacyReservationConflict"
		return v
	}
	v.AccountingKnown = true
	v.Used = corev1.ResourceList{}
	for key, amount := range v.Capacity {
		if held != nil && held.Spec.Upgrade == nil {
			v.Used[key] = amount.DeepCopy()
		} else {
			v.Used[key] = *resource.NewQuantity(0, resource.DecimalSI)
		}
	}
	if v.Capacity == nil {
		return v
	}
	if !Fresh(cn.Status.ResourceObservedAt, now) || !Fresh(cn.Status.LastHeartbeatTime, now) || cn.Status.LastHeartbeatTime.Before(cn.Status.ResourceObservedAt) {
		v.Reason = "ResourceObservationStale"
		return v
	}
	if cn.Status.AgentCredentialID == "" || cn.Status.AgentCredentialID != cn.Annotations[ext.AgentCredentialAnnotation] || cn.Status.ObservedGeneration != cn.Generation || !current(cn.Status.Conditions, "Ready", cn.Generation) || !current(cn.Status.Conditions, "Validated", cn.Generation) {
		v.Reason = "AgentObservationInvalid"
		return v
	}
	v.ResourcesValid = true
	switch {
	case !cn.DeletionTimestamp.IsZero() || !nn.DeletionTimestamp.IsZero() || !cc.DeletionTimestamp.IsZero() || !nc.DeletionTimestamp.IsZero():
		v.Reason = "Deleting"
	case held != nil && held.Spec.Upgrade != nil:
		v.Reason = "MaintenanceHold"
	case held != nil:
		v.Reason = "WorkloadReserved"
	case cn.Status.Busy:
		v.Reason = "AgentBusy"
	case nn.Spec.Unschedulable:
		v.Reason = "NodeCordoned"
	case nc.Spec.Cordoned || nc.Spec.Draining:
		v.Reason = "ClusterUnavailable"
	case !releaseReady:
		v.Reason = "AgentReleasePending"
	case !cn.Status.Schedulable:
		v.Reason = "AgentNotSchedulable"
	default:
		v.Schedulable, v.Reason = true, "RawNodeAvailable"
	}
	return v
}

func sameGPUs(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b) && len(slices.Compact(a)) == len(a)
}

// ReleaseReady verifies the current desired target rather than a previous
// Agent's cached interpretation of that target. Nil is valid only without one.
func ReleaseReady(cn *ext.ContainerNode, cc *ext.ContainerCluster, release *ext.AgentRelease) bool {
	if cc.Spec.TargetAgentRelease == "" {
		return true
	}
	return release != nil && release.Name == cc.Spec.TargetAgentRelease && release.Namespace == cc.Namespace && release.UID != "" && release.DeletionTimestamp.IsZero() && tenant.IDOf(release) == tenant.IDOf(cn) && artifact.ValidateSpec(release.Spec) == nil && current(release.Status.Conditions, "Available", release.Generation) && release.Status.Available && release.Status.ObservedGeneration == release.Generation && release.Spec.Version == cn.Status.AgentVersion && cn.Status.TargetAgentVersion == release.Spec.Version && current(cn.Status.Conditions, "AgentReleaseReady", cn.Generation)
}

func (v View) Message() string {
	return fmt.Sprintf("Raw Notebook resource state: %s; capacity is a cgroup bound, used is an exclusive RLark workload reservation", v.Reason)
}
