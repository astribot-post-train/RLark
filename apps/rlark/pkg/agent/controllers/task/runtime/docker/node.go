package docker

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
)

const nodeObserveInterval = 15 * time.Second

// NodeObserver projects Docker Engine capacity and liveness into the shared
// RLark Node resource. It never changes Node.Spec.unschedulable and refuses to
// overwrite a Node claimed by another runtime.
type NodeObserver struct {
	Client        client.Client
	Engine        EngineClient
	Node          client.ObjectKey
	NodeIP        string
	NodeUID       string
	Incarnation   string
	GPUDiscoverer func(context.Context) (int64, error)
}

func (o *NodeObserver) Start(ctx context.Context) error {
	if o == nil || o.Client == nil || o.Engine == nil || o.Node.Namespace == "" || o.Node.Name == "" {
		return fmt.Errorf("Docker node observer is not configured")
	}
	if err := o.observe(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(nodeObserveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := o.observe(ctx); err != nil {
				// A transient Docker outage should make the Node stale through the
				// control-plane heartbeat rather than terminate the Agent process.
				continue
			}
		}
	}
}

func (o *NodeObserver) observe(ctx context.Context) error {
	info, err := o.Engine.Info(ctx)
	if err != nil {
		return fmt.Errorf("inspect Docker Engine: %w", err)
	}
	version, _ := o.Engine.ServerVersion(ctx)
	var node api.Node
	if err := o.Client.Get(ctx, o.Node, &node); err != nil {
		return fmt.Errorf("read Docker Node: %w", err)
	}
	if node.Spec.AgentType != "" && node.Spec.AgentType != api.AgentTypeDocker {
		return fmt.Errorf("Node %s is owned by agent type %s", o.Node.String(), node.Spec.AgentType)
	}
	if o.NodeUID != "" && string(node.UID) != o.NodeUID {
		return fmt.Errorf("Docker Node UID changed from %s to %s", o.NodeUID, node.UID)
	}
	desired := node.DeepCopy()
	desired.Spec.AgentType = api.AgentTypeDocker
	desired.Labels = copyLabels(desired.Labels)
	desired.Labels[api.LabelAgentType] = string(api.AgentTypeDocker)
	if o.NodeIP != "" {
		desired.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: o.NodeIP}}
	} else if host := info.Name; host != "" {
		desired.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeHostName, Address: host}}
	}
	desired.Status.Phase = api.NodeOnline
	desired.Status.Reason = "DockerEngineReady"
	capacity := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewQuantity(int64(info.NCPU), resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(info.MemTotal, resource.BinarySI),
	}
	gpuDiscoverer := o.GPUDiscoverer
	if gpuDiscoverer == nil {
		gpuDiscoverer = discoverGPUCount
	}
	if gpu, gpuErr := gpuDiscoverer(ctx); gpuErr == nil && gpu >= 0 {
		capacity[corev1.ResourceName("nvidia.com/gpu")] = *resource.NewQuantity(gpu, resource.DecimalSI)
	}
	desired.Status.Capacity = capacity
	desired.Status.Allocatable = capacity.DeepCopy()
	desired.Status.NodeInfo = api.NodeInfo{Architecture: info.Architecture, KernelVersion: info.KernelVersion, OperatingSystem: info.OSType, AgentVersion: strings.TrimSpace(version.Version), Incarnation: info.ID}
	if desired.Spec.AgentType != node.Spec.AgentType || desired.ResourceVersion == "" || !sameLabels(node.Labels, desired.Labels) {
		if err := o.Client.Update(ctx, desired); err != nil {
			return fmt.Errorf("update Docker Node: %w", err)
		}
		// Update advances ResourceVersion. Re-read before the status patch so a
		// concurrent controller-owned reservation is not patched from a stale
		// object.
		if err := o.Client.Get(ctx, o.Node, &node); err != nil {
			return fmt.Errorf("re-read Docker Node after update: %w", err)
		}
	}
	before := node.DeepCopy()
	// Preserve controller-owned reservations and any unrelated status fields.
	merged := node.Status
	merged.Phase = desired.Status.Phase
	merged.Reason = desired.Status.Reason
	merged.Addresses = desired.Status.Addresses
	merged.Capacity = desired.Status.Capacity
	merged.Allocatable = desired.Status.Allocatable
	merged.NodeInfo = desired.Status.NodeInfo
	node.Status = merged
	if err := o.Client.Status().Patch(ctx, &node, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("patch Docker Node status: %w", err)
	}
	return nil
}

func discoverGPUCount(ctx context.Context) (int64, error) {
	output, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=uuid", "--format=csv,noheader").Output()
	if err != nil {
		return 0, err
	}
	count := int64(0)
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count, nil
}

func copyLabels(values map[string]string) map[string]string {
	result := make(map[string]string, len(values)+1)
	for key, value := range values {
		result[key] = value
	}
	return result
}
func sameLabels(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

func privateIP(value string) bool {
	ip := net.ParseIP(value)
	return ip != nil && ip.IsPrivate()
}
