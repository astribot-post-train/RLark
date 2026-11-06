package task

import (
	"context"
	"fmt"

	"github.com/rlinf/rlark/api/metadata"
	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Read the preview annotation for existing Tasks; new writers use metadata's
// canonical key. Conflicting keys are never silently normalized or adopted.
const legacyTargetClusterUID = "containers.rlark.io/target-cluster-uid"

func hasKubernetesTarget(task *api.Task) bool {
	_, canonical := task.Annotations[metadata.TargetClusterUIDAnnotation]
	_, legacy := task.Annotations[legacyTargetClusterUID]
	return canonical || legacy
}

// kubernetesTaskClient fences claims as well as workload effects. The Host's
// deletion path retains its existing UID/claim ownership cleanup contract.
type kubernetesTaskClient struct {
	client.Client
	namespace string
}

func (c kubernetesTaskClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, object, options...); err != nil {
		return err
	}
	task, ok := object.(*api.Task)
	if !ok || !task.DeletionTimestamp.IsZero() {
		return nil
	}
	return validateKubernetesTarget(ctx, c.Client, c.namespace, task)
}

// validateKubernetesTarget uses an uncached management read. Namespace membership
// comes from Cluster.spec.agentNamespace, never labels or a guessed Cluster name.
// Unpinned historical Tasks preserve their existing admission contract.
func validateKubernetesTarget(ctx context.Context, reader client.Reader, namespace string, task *api.Task) error {
	if task == nil {
		return fault.New(fault.PermissionDenied, "Kubernetes Task identity is absent")
	}
	if !hasKubernetesTarget(task) {
		return nil
	}
	deny := func() error {
		return fault.New(fault.PermissionDenied, "Kubernetes Task target Cluster identity or tenant conflicts")
	}
	uid, canonical := task.Annotations[metadata.TargetClusterUIDAnnotation]
	old, legacy := task.Annotations[legacyTargetClusterUID]
	if !canonical {
		uid = old
	}
	if uid == "" || canonical && legacy && old != uid || namespace == "" || task.Namespace != namespace || task.UID == "" || task.Spec.Runtime.Kind != api.RuntimeKindKubernetes {
		return deny()
	}
	var clusters api.ClusterList
	if err := reader.List(ctx, &clusters); err != nil {
		return fmt.Errorf("read Kubernetes Task target Clusters: %w", err)
	}
	owners := 0
	for i := range clusters.Items {
		cluster := &clusters.Items[i]
		if cluster.Spec.AgentNamespace != namespace {
			continue
		}
		owners++
		if string(cluster.UID) != uid || metadata.IDOf(cluster) != metadata.IDOf(task) || !cluster.DeletionTimestamp.IsZero() {
			return deny()
		}
	}
	if owners != 1 {
		return deny()
	}
	return nil
}
