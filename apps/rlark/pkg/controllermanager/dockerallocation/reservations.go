// Package dockerallocation implements durable, whole-GPU Docker host
// reservations. The Kubernetes API is the lock and recovery record; no
// in-memory scheduler state is authoritative.
package dockerallocation

import (
	"context"
	"errors"
	"fmt"
	"sort"

	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var ErrUnavailable = errors.New("Docker node capacity unavailable")

type Request struct {
	Cluster      client.ObjectKey
	JobUID       string
	TaskUID      string
	TenantID     string
	NodeSelector map[string]string
	CPUMilli     int64
	MemoryBytes  int64
	GPUCount     int64
}

type Manager struct{ Client client.Client }

func (m Manager) Reserve(ctx context.Context, req Request) (api.DockerTaskBinding, error) {
	if m.Client == nil || req.JobUID == "" || req.TaskUID == "" || req.TenantID == "" {
		return api.DockerTaskBinding{}, errors.New("complete Docker allocation identity is required")
	}
	var cluster api.Cluster
	if err := m.Client.Get(ctx, req.Cluster, &cluster); err != nil {
		return api.DockerTaskBinding{}, err
	}
	if cluster.UID == "" || cluster.Spec.AgentNamespace == "" || cluster.Spec.Cordoned || cluster.Spec.Draining || !cluster.DeletionTimestamp.IsZero() || tenant.IDOf(&cluster) != req.TenantID {
		return api.DockerTaskBinding{}, fmt.Errorf("%w: Cluster is not eligible", ErrUnavailable)
	}
	var clusters api.ClusterList
	if err := m.Client.List(ctx, &clusters); err != nil {
		return api.DockerTaskBinding{}, err
	}
	for _, other := range clusters.Items {
		if other.UID != cluster.UID && other.Spec.AgentNamespace == cluster.Spec.AgentNamespace {
			return api.DockerTaskBinding{}, errors.New("ambiguous Cluster agent namespace")
		}
	}
	var nodes api.NodeList
	if err := m.Client.List(ctx, &nodes, client.InNamespace(cluster.Spec.AgentNamespace)); err != nil {
		return api.DockerTaskBinding{}, err
	}
	selector := labels.SelectorFromSet(req.NodeSelector)
	sort.Slice(nodes.Items, func(i, j int) bool { return nodes.Items[i].Name < nodes.Items[j].Name })
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if !selector.Matches(labels.Set(node.Labels)) || node.Spec.AgentType != api.AgentTypeDocker || node.Spec.Unschedulable || node.Status.Phase != api.NodeOnline || node.UID == "" || !node.DeletionTimestamp.IsZero() {
			continue
		}
		if tenant.IDOf(node) != req.TenantID || node.Status.NodeInfo.Incarnation == "" {
			continue
		}
		for _, current := range node.Status.DockerReservations {
			if current.TaskUID == req.TaskUID {
				return bindingFor(node, current), nil
			}
		}
		if !fits(node, req.CPUMilli, req.MemoryBytes, req.GPUCount) {
			continue
		}
		reservation := api.DockerNodeReservation{TaskUID: req.TaskUID, JobUID: req.JobUID, TenantID: req.TenantID, NodeUID: string(node.UID), ClusterUID: string(cluster.UID), Incarnation: node.Status.NodeInfo.Incarnation, AllocationID: req.TaskUID + ":" + string(node.UID), CPUMilli: req.CPUMilli, MemoryBytes: req.MemoryBytes, GPUCount: req.GPUCount}
		before := node.DeepCopy()
		node.Status.DockerReservations = append(node.Status.DockerReservations, reservation)
		if err := m.Client.Status().Patch(ctx, node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return api.DockerTaskBinding{}, err
		}
		return bindingFor(node, reservation), nil
	}
	return api.DockerTaskBinding{}, ErrUnavailable
}

func fits(node *api.Node, cpu, memory, gpu int64) bool {
	usedCPU, usedMemory, usedGPU := int64(0), int64(0), int64(0)
	for _, r := range node.Status.DockerReservations {
		usedCPU += r.CPUMilli
		usedMemory += r.MemoryBytes
		usedGPU += r.GPUCount
	}
	cpuQuantity := node.Status.Allocatable[corev1.ResourceCPU]
	memoryQuantity := node.Status.Allocatable[corev1.ResourceMemory]
	gpuQuantity := node.Status.Allocatable[corev1.ResourceName("nvidia.com/gpu")]
	capCPU := cpuQuantity.MilliValue()
	capMemory := memoryQuantity.Value()
	capGPU := gpuQuantity.Value()
	return cpu >= 0 && memory >= 0 && gpu >= 0 && usedCPU+cpu <= capCPU && usedMemory+memory <= capMemory && usedGPU+gpu <= capGPU
}

func bindingFor(node *api.Node, r api.DockerNodeReservation) api.DockerTaskBinding {
	return api.DockerTaskBinding{TaskUID: r.TaskUID, NodeName: node.Name, NodeUID: r.NodeUID, Incarnation: r.Incarnation, JobUID: r.JobUID, ClusterUID: r.ClusterUID, AllocationID: r.AllocationID}
}

func (m Manager) ReleaseAt(ctx context.Context, key client.ObjectKey, binding api.DockerTaskBinding, tenantID string) error {
	var node api.Node
	if err := m.Client.Get(ctx, key, &node); err != nil {
		return client.IgnoreNotFound(err)
	}
	return m.releaseNode(ctx, &node, binding, tenantID)
}

func (m Manager) releaseNode(ctx context.Context, node *api.Node, binding api.DockerTaskBinding, tenantID string) error {
	if string(node.UID) != binding.NodeUID || tenant.IDOf(node) != tenantID || node.Status.NodeInfo.Incarnation != binding.Incarnation {
		return errors.New("Docker cleanup identity conflict")
	}
	for i, current := range node.Status.DockerReservations {
		if current.TaskUID != binding.TaskUID {
			continue
		}
		if current.JobUID != binding.JobUID || current.AllocationID != binding.AllocationID || current.NodeUID != binding.NodeUID || current.ClusterUID != binding.ClusterUID {
			return errors.New("Docker reservation ownership conflict")
		}
		before := node.DeepCopy()
		node.Status.DockerReservations = append(node.Status.DockerReservations[:i], node.Status.DockerReservations[i+1:]...)
		return m.Client.Status().Patch(ctx, node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}
	return nil
}

func ValidateBinding(node *api.Node, binding *api.DockerTaskBinding, tenantID string) error {
	if node == nil || binding == nil || tenantID == "" || string(node.UID) != binding.NodeUID || node.Spec.AgentType != api.AgentTypeDocker || node.Status.Phase != api.NodeOnline || tenant.IDOf(node) != tenantID || node.Status.NodeInfo.Incarnation != binding.Incarnation {
		return errors.New("Docker binding does not match node")
	}
	for _, r := range node.Status.DockerReservations {
		if r.TaskUID == binding.TaskUID && r.JobUID == binding.JobUID && r.NodeUID == binding.NodeUID && r.ClusterUID == binding.ClusterUID && r.AllocationID == binding.AllocationID && r.Incarnation == binding.Incarnation {
			return nil
		}
	}
	return errors.New("Docker reservation is missing or conflicts")
}
