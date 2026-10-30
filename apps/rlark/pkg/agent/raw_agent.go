package agent

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"

	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers"
	rawruntime "github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/runtime/raw"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/runtime/raw/supervision"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/host"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/workload"
	"github.com/rlinf/rlark/apps/rlark/pkg/log"
	"github.com/rlinf/rlark/apps/rlark/pkg/rawnode"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (c Config) rawCapability(incarnation string) *api.RawNodeStatus {
	return &api.RawNodeStatus{Incarnation: incarnation, TrainingIP: c.RawTrainingIP, NetworkScope: c.RawNetworkScope, Environment: c.RawEnvironment, AuthorizedGPUUUIDs: slices.Clone(c.RawAuthorizedGPUUUIDs)}
}
func (c Config) validateRaw() error {
	if c.AgentType != string(api.AgentTypeRaw) {
		return nil
	}
	if c.Mode != "cluster" || c.RawSupervisorSocket == "" {
		return fmt.Errorf("Raw requires --mode=cluster and an independent --raw-supervisor-socket")
	}
	return rawnode.ValidateCapability(c.rawCapability("configured"))
}

// runRaw deliberately uses no discovery, informer or watch: the restricted API
// supports direct reads and versioned writes. Polling repairs lost notifications.
func (c *clusterAgent) runRaw(ctx context.Context) error {
	identity, err := rawIdentityFromClientCertificate(c.a.config.ClientConfig)
	if err != nil {
		return fmt.Errorf("load Raw node identity: %w", err)
	}
	if identity.Namespace != c.a.config.ClientConfig.ServerNamespace {
		return fmt.Errorf("Raw certificate namespace differs from configured namespace")
	}
	management, err := client.New(c.a.managementConfig, client.Options{Scheme: controllers.MgmtScheme, Mapper: rawnode.RESTMapper()})
	if err != nil {
		return fmt.Errorf("create scoped Raw client: %w", err)
	}
	supervisor, err := supervision.NewClient(c.a.config.RawSupervisorSocket)
	if err != nil {
		return err
	}
	admission := rawruntime.NodeAdmission{Client: management, Node: client.ObjectKey{Namespace: identity.Namespace, Name: identity.Node}, NodeUID: identity.NodeUID, Incarnation: identity.Incarnation, TenantID: identity.TenantID}
	h, err := rawruntime.NewHost(ctx, management, admission, &rawruntime.Provider{ID: "raw-node", Supervisor: supervisor}, []workload.Adapter{rawruntime.ProcessAdapter{}})
	if err != nil {
		return err
	}
	// Invalid local interface/device selection fails before heartbeats advertise
	// readiness. Temporary API outages are retried by the loop below.
	capability := c.a.config.rawCapability(identity.Incarnation)
	if err := validateLocalRawCapability(ctx, capability); err != nil {
		return err
	}
	eg, runCtx := errgroup.WithContext(ctx)
	eg.Go(func() error { runRawHeartbeat(runCtx, c.a.serverClient); return nil })
	eg.Go(func() error {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			scanCtx, cancel := context.WithTimeout(runCtx, 30*time.Second)
			err := reconcileRawScan(scanCtx, management, h, identity, capability)
			cancel()
			if err != nil && runCtx.Err() == nil {
				log.FromContext(ctx).Error(err, "Raw reconciliation scan failed; will retry")
			}
			select {
			case <-runCtx.Done():
				return nil
			case <-ticker.C:
			}
		}
	})
	return eg.Wait()
}

func validateLocalRawCapability(ctx context.Context, v *api.RawNodeStatus) error {
	if err := rawnode.ValidateCapability(v); err != nil {
		return err
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return fmt.Errorf("read local addresses: %w", err)
	}
	found := false
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address.String())
		if err == nil && ip.Equal(net.ParseIP(v.TrainingIP)) {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("Raw training IP is not assigned to this container")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(probeCtx, "nvidia-smi", "--query-gpu=uuid", "--format=csv,noheader").Output()
	if err != nil {
		return fmt.Errorf("observe Raw GPU UUIDs: %w", err)
	}
	visible := strings.Fields(string(output))
	for _, id := range v.AuthorizedGPUUUIDs {
		if !slices.Contains(visible, id) {
			return fmt.Errorf("an authorized Raw GPU is not visible in this container")
		}
	}
	return nil
}

func reconcileRawScan(ctx context.Context, management client.Client, h *host.Host, i rawnode.Identity, capability *api.RawNodeStatus) error {
	var node api.Node
	if err := management.Get(ctx, client.ObjectKey{Namespace: i.Namespace, Name: i.Node}, &node); err != nil {
		return err
	}
	authorized, err := rawnode.AuthorizedCapability(&node)
	if err != nil {
		return err
	}
	if string(node.UID) != i.NodeUID || !rawnode.SameCapability(authorized, capability) {
		return fmt.Errorf("Raw capability conflicts with administrator authorization")
	}
	before := node.DeepCopy()
	node.Status.Raw = capability
	node.Status.NodeInfo = api.NodeInfo{OperatingSystem: runtime.GOOS, Architecture: runtime.GOARCH}
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: capability.TrainingIP}}
	// CPU and memory are omitted until the container's effective cgroup limits
	// can be proved. Host-visible totals must never masquerade as entitlement.
	gpu := corev1.ResourceName("nvidia.com/gpu")
	node.Status.Capacity = corev1.ResourceList{gpu: *resource.NewQuantity(int64(len(capability.AuthorizedGPUUUIDs)), resource.DecimalSI)}
	node.Status.Allocatable = node.Status.Capacity.DeepCopy()
	node.Status.Used = nil
	if !equality.Semantic.DeepEqual(before.Status, node.Status) {
		if err := management.Status().Patch(ctx, &node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("report Raw capabilities: %w", err)
		}
	}
	var tasks api.TaskList
	if err := management.List(ctx, &tasks, client.InNamespace(i.Namespace)); err != nil {
		return err
	}
	for idx := range tasks.Items {
		task := &tasks.Items[idx]
		if !h.ShouldReconcile(task) {
			continue
		}
		taskCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := h.Reconcile(taskCtx, client.ObjectKeyFromObject(task))
		cancel()
		if err != nil {
			log.FromContext(ctx).Error(err, "Raw Task reconciliation failed; will retry", "task", task.Name)
		}
	}
	return nil
}
