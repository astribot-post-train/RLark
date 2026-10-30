package docker

import (
	"context"
	"errors"
	"fmt"

	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/dockerallocation"
	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	"github.com/rlinf/rlark/apps/rlark/pkg/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NodeAdmission revalidates the durable Task binding immediately before a
// container is created. The Docker daemon is never treated as an authority for
// management ownership; the management API remains the source of truth.
type NodeAdmission struct {
	Client      client.Client
	Node        client.ObjectKey
	NodeUID     string
	Incarnation string
	TenantID    string
}

func (a NodeAdmission) Authorize(ctx context.Context, identity rt.TaskIdentity) error {
	if a.Client == nil || a.Node.Namespace == "" || a.Node.Name == "" || a.NodeUID == "" || a.Incarnation == "" || a.TenantID == "" {
		return errors.New("Docker node admission identity is incomplete")
	}
	deny := func() error { return fault.New(fault.PermissionDenied, "Docker Task is not authorized for this node") }
	if identity.Logical.Namespace != a.Node.Namespace {
		return deny()
	}
	var task api.Task
	if err := a.Client.Get(ctx, client.ObjectKey{Namespace: identity.Logical.Namespace, Name: identity.Logical.Name}, &task); err != nil {
		return fmt.Errorf("read admitted Docker Task: %w", err)
	}
	b := task.Spec.Placement.DockerBinding
	if task.UID == "" || string(task.UID) != identity.ObjectUID || task.Generation != identity.Generation || !task.DeletionTimestamp.IsZero() || task.Spec.Suspended || task.Spec.Runtime.Kind != api.RuntimeKind(RuntimeKind) || b == nil || b.TaskUID != identity.ObjectUID || b.NodeName != a.Node.Name || b.NodeUID != a.NodeUID || b.Incarnation != a.Incarnation || tenant.IDOf(&task) != a.TenantID {
		return deny()
	}
	owner := metav1.GetControllerOf(&task)
	if owner == nil || owner.Kind != "Job" || owner.APIVersion != api.GroupVersion.String() || string(owner.UID) != b.JobUID || task.Annotations[utils.ParentUIDAnnotation] != b.JobUID {
		return deny()
	}
	var job api.Job
	if err := a.Client.Get(ctx, client.ObjectKey{Name: owner.Name}, &job); err != nil {
		return fmt.Errorf("read admitted Docker Job: %w", err)
	}
	if string(job.UID) != b.JobUID || tenant.IDOf(&job) != a.TenantID || job.Spec.Suspended || !job.DeletionTimestamp.IsZero() {
		return deny()
	}
	var node api.Node
	if err := a.Client.Get(ctx, a.Node, &node); err != nil {
		return fmt.Errorf("read admitted Docker Node: %w", err)
	}
	if string(node.UID) != a.NodeUID || node.Spec.AgentType != api.AgentTypeDocker || node.Status.Phase != api.NodeOnline || node.Status.NodeInfo.Incarnation != a.Incarnation {
		return deny()
	}
	if err := dockerallocation.ValidateBinding(&node, b, a.TenantID); err != nil {
		return deny()
	}
	var clusters api.ClusterList
	if err := a.Client.List(ctx, &clusters); err != nil {
		return fmt.Errorf("read admitted Docker Cluster: %w", err)
	}
	owners := 0
	for _, cluster := range clusters.Items {
		if cluster.Spec.AgentNamespace != task.Namespace {
			continue
		}
		owners++
		if string(cluster.UID) != b.ClusterUID || tenant.IDOf(&cluster) != a.TenantID || !cluster.DeletionTimestamp.IsZero() {
			return deny()
		}
	}
	if owners != 1 {
		return deny()
	}
	return nil
}
