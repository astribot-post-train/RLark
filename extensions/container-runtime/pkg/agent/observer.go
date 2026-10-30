// Package agent contains the scoped observation API for container nodes.
// Device visibility never grants scheduling capacity.
package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/ownership"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/rawnode"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CredentialAnnotation is rotated by the administrator before credentials are
// issued. An old Agent cannot make a replacement node appear ready.
const CredentialAnnotation = ext.AgentCredentialAnnotation

var (
	// ErrIdentity rejects stale, replaced, ambiguous or cross-tenant bindings.
	ErrIdentity = errors.New("container Agent identity conflict")
	// ErrCapability rejects an observation outside provisioned capability.
	ErrCapability = errors.New("container Agent capability is not authorized")
	// ErrUnavailable reports a dependency whose current state cannot be proven.
	ErrUnavailable = errors.New("container Agent observation unavailable")
)

// Identity binds an authenticated request to one exact companion object and
// its current container incarnation. Names are hints; UIDs are authoritative.
type Identity struct {
	Namespace    string
	Name         string
	UID          string
	ClusterUID   string
	InstanceID   string
	TenantID     string
	CredentialID string
}

// Observation is supplied by a trusted Agent after local checks. Capacity is
// a bounded snapshot, separate from the operator-authorized GPU allowlist.
type Observation struct {
	Version         string            `json:"version"`
	PrivateAddress  string            `json:"privateAddress"`
	Capacity        map[string]string `json:"capacity,omitempty"`
	Capabilities    []string          `json:"capabilities,omitempty"`
	VisibleGPUUUIDs []string          `json:"visibleGPUUUIDs,omitempty"`
	Busy            bool              `json:"busy"`
}

// Reporter writes Agent-owned status using an uncached client. It never edits
// Spec, native Nodes, allocations, or another Agent's identity.
type Reporter struct {
	Client client.Client
	Now    func() time.Time
}

func (r Reporter) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Report validates the full binding and records a current observation. The
// caller must derive id from authenticated transport, never a request body.
// A target release fences scheduling until its available version is observed.
func (r Reporter) Report(ctx context.Context, id Identity, obs Observation) error {
	if r.Client == nil || id.Namespace == "" || id.Name == "" || id.UID == "" || id.ClusterUID == "" || id.InstanceID == "" || id.TenantID == "" || id.CredentialID == "" {
		return ErrIdentity
	}
	var node ext.ContainerNode
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: id.Namespace, Name: id.Name}, &node); err != nil {
		return err
	}
	if string(node.UID) != id.UID || node.Namespace != id.Namespace || node.Spec.InstanceID != id.InstanceID || tenant.IDOf(&node) != id.TenantID || node.Annotations[CredentialAnnotation] != id.CredentialID || !node.DeletionTimestamp.IsZero() {
		return ErrIdentity
	}
	if err := r.validateBindings(ctx, &node, id); err != nil {
		return err
	}
	if err := validateObservation(&node, obs); err != nil {
		return err
	}
	target, releaseReady, err := r.releaseFence(ctx, &node, obs.Version)
	if err != nil {
		return err
	}
	before := node.DeepCopy()
	node.Status.ObservedGeneration = node.Generation
	node.Status.AgentCredentialID = id.CredentialID
	node.Status.AgentVersion = obs.Version
	node.Status.TargetAgentVersion = target
	node.Status.PrivateAddress = obs.PrivateAddress
	node.Status.Capacity = cloneMap(obs.Capacity)
	if node.Status.Capacity == nil {
		node.Status.Capacity = map[string]string{}
	}
	node.Status.Capacity["nvidia.com/gpu"] = strconv.Itoa(len(node.Spec.AuthorizedGPUUUIDs))
	node.Status.Capabilities = slices.Clone(obs.Capabilities)
	node.Status.AuthorizedGPUUUIDs = slices.Clone(node.Spec.AuthorizedGPUUUIDs)
	node.Status.Schedulable = releaseReady && !obs.Busy
	node.Status.Busy = obs.Busy
	now := metav1.NewTime(r.now())
	node.Status.LastHeartbeatTime = &now
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "AgentObserved", Message: "Authenticated Agent reported an authorized capability", ObservedGeneration: node.Generation, LastTransitionTime: now})
	releaseStatus, releaseReason := metav1.ConditionTrue, "AgentReleaseObserved"
	if !releaseReady {
		releaseStatus, releaseReason = metav1.ConditionFalse, "AgentReleasePending"
	}
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{Type: "AgentReleaseReady", Status: releaseStatus, Reason: releaseReason, Message: "Scheduling requires the current available target Agent release", ObservedGeneration: node.Generation, LastTransitionTime: now})
	if err := r.Client.Status().Patch(ctx, &node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("patch container Node observation: %w", err)
	}
	return nil
}

func (r Reporter) validateBindings(ctx context.Context, node *ext.ContainerNode, id Identity) error {
	validated := meta.FindStatusCondition(node.Status.Conditions, "Validated")
	if validated == nil || validated.Status != metav1.ConditionTrue || validated.ObservedGeneration != node.Generation {
		return ErrUnavailable
	}
	var cluster native.Cluster
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: node.Spec.Cluster.Namespace, Name: node.Spec.Cluster.Name}, &cluster); err != nil {
		return err
	}
	if err := ownership.MatchesNative(node.Spec.Cluster, &cluster); err != nil || string(cluster.UID) != id.ClusterUID || cluster.Spec.AgentNamespace != node.Namespace || tenant.IDOf(&cluster) != id.TenantID || !cluster.DeletionTimestamp.IsZero() {
		return ErrIdentity
	}
	var nativeNode native.Node
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: node.Spec.Node.Namespace, Name: node.Spec.Node.Name}, &nativeNode); err != nil {
		return err
	}
	if err := ownership.MatchesNative(node.Spec.Node, &nativeNode); err != nil || nativeNode.Namespace != node.Namespace || tenant.IDOf(&nativeNode) != id.TenantID || string(nativeNode.Spec.AgentType) != string(node.Spec.Runtime) || !nativeNode.DeletionTimestamp.IsZero() || nativeNode.Status.Reservation != nil || len(nativeNode.Status.DockerReservations) != 0 {
		return ErrIdentity
	}
	var nativeClusters native.ClusterList
	if err := r.Client.List(ctx, &nativeClusters); err != nil {
		return err
	}
	for _, other := range nativeClusters.Items {
		if other.UID != cluster.UID && other.Spec.AgentNamespace == cluster.Spec.AgentNamespace {
			return ErrIdentity
		}
	}
	var clusters ext.ContainerClusterList
	if err := r.Client.List(ctx, &clusters, client.InNamespace(node.Namespace)); err != nil {
		return err
	}
	count := 0
	for i := range clusters.Items {
		cc := &clusters.Items[i]
		if cc.Spec.Cluster == node.Spec.Cluster {
			count++
			validated := meta.FindStatusCondition(cc.Status.Conditions, "Validated")
			if cc.Spec.Runtime != node.Spec.Runtime || cc.Spec.AgentNamespace != node.Namespace || tenant.IDOf(cc) != id.TenantID || !cc.DeletionTimestamp.IsZero() || validated == nil || validated.Status != metav1.ConditionTrue || validated.ObservedGeneration != cc.Generation || cc.Status.ObservedGeneration != cc.Generation {
				return ErrUnavailable
			}
		}
	}
	if count != 1 {
		return ErrIdentity
	}
	var inventory ext.ContainerNodeList
	if err := r.Client.List(ctx, &inventory); err != nil {
		return err
	}
	for _, other := range inventory.Items {
		if other.UID == node.UID {
			continue
		}
		if other.Spec.Node == node.Spec.Node || other.Spec.NetworkScope == node.Spec.NetworkScope && other.Spec.PrivateAddress == node.Spec.PrivateAddress {
			return ErrIdentity
		}
		for _, gpu := range other.Spec.AuthorizedGPUUUIDs {
			if slices.Contains(node.Spec.AuthorizedGPUUUIDs, gpu) {
				return ErrIdentity
			}
		}
	}
	return nil
}

func validateObservation(node *ext.ContainerNode, obs Observation) error {
	if len(obs.Version) > 128 || strings.TrimSpace(obs.Version) == "" || strings.TrimSpace(obs.PrivateAddress) == "" || obs.PrivateAddress != node.Spec.PrivateAddress || len(obs.Capabilities) > 16 || len(obs.VisibleGPUUUIDs) > 256 {
		return ErrCapability
	}
	ip := net.ParseIP(obs.PrivateAddress)
	if ip == nil || ip.To4() == nil || !ip.IsPrivate() {
		return ErrCapability
	}
	if err := rawnode.ValidateCapability(&native.RawNodeStatus{Incarnation: node.Spec.InstanceID, TrainingIP: node.Spec.PrivateAddress, NetworkScope: node.Spec.NetworkScope, Environment: node.Spec.Environment, AuthorizedGPUUUIDs: node.Spec.AuthorizedGPUUUIDs}); err != nil {
		return fmt.Errorf("%w: %v", ErrCapability, err)
	}
	seen := map[string]struct{}{}
	for _, gpu := range obs.VisibleGPUUUIDs {
		if gpu == "" {
			return ErrCapability
		}
		if _, ok := seen[gpu]; ok {
			return ErrCapability
		}
		seen[gpu] = struct{}{}
	}
	for _, gpu := range node.Spec.AuthorizedGPUUUIDs {
		if _, ok := seen[gpu]; !ok {
			return fmt.Errorf("%w: authorized GPU %q is not visible", ErrCapability, gpu)
		}
	}
	for key, value := range obs.Capacity {
		if key != "cpu" && key != "memory" && key != "nvidia.com/gpu" || len(value) > 64 {
			return ErrCapability
		}
		q, err := resource.ParseQuantity(value)
		if err != nil || q.Sign() < 0 {
			return ErrCapability
		}
		if key == "nvidia.com/gpu" && (q.MilliValue()%1000 != 0 || q.Value() != int64(len(node.Spec.AuthorizedGPUUUIDs))) {
			return ErrCapability
		}
	}
	for _, capability := range obs.Capabilities {
		if len(capability) == 0 || len(capability) > 128 || strings.ContainsAny(capability, "\x00\r\n") {
			return ErrCapability
		}
	}
	return nil
}

func (r Reporter) releaseFence(ctx context.Context, node *ext.ContainerNode, version string) (string, bool, error) {
	var clusters ext.ContainerClusterList
	if err := r.Client.List(ctx, &clusters, client.InNamespace(node.Namespace)); err != nil {
		return "", false, err
	}
	for i := range clusters.Items {
		cc := &clusters.Items[i]
		if cc.Spec.Cluster != node.Spec.Cluster || cc.Spec.TargetAgentRelease == "" {
			continue
		}
		var release ext.AgentRelease
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: cc.Namespace, Name: cc.Spec.TargetAgentRelease}, &release); err != nil {
			if apierrors.IsNotFound(err) {
				return "", false, nil
			}
			return "", false, err
		}
		return release.Spec.Version, release.DeletionTimestamp.IsZero() && release.Status.Available && release.Status.ObservedGeneration == release.Generation && release.Spec.Version == version, nil
	}
	return "", true, nil
}

func cloneMap(v map[string]string) map[string]string {
	if v == nil {
		return nil
	}
	result := make(map[string]string, len(v))
	for k, value := range v {
		result[k] = value
	}
	return result
}
