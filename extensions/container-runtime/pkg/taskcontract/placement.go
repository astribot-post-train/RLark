package taskcontract

import (
	"context"
	"errors"
	"fmt"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/core/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/core/pkg/ownership"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrInvalidTarget identifies a conflicting or unsupported placement contract.
// API read failures retain their original error for retry by the caller.
var ErrInvalidTarget = errors.New("invalid Kubernetes target")

// KubernetesTargets revalidates explicit target instances and resolves their
// authoritative Agent namespaces. It is shared by scheduling and Raw dispatch;
// neither a status condition nor user-provided namespace grants placement.
func KubernetesTargets(ctx context.Context, reader client.Reader, b *ext.ContainerJob, job *native.Job) (map[string]string, error) {
	invalid := func(message string) (map[string]string, error) {
		return nil, fmt.Errorf("%w: %s", ErrInvalidTarget, message)
	}
	namespaces := map[string]string{}
	if len(b.Spec.KubernetesTargets) == 0 {
		for _, t := range job.Spec.Tasks {
			if string(t.Runtime.Kind) != string(b.Spec.Runtime) {
				return invalid("mixed template requires an explicit Kubernetes target")
			}
		}
		return namespaces, nil
	}
	if b.Spec.Runtime != "Raw" || ValidateMixedJob(job) != nil {
		return invalid("unsupported mixed Job contract")
	}
	var clusters native.ClusterList
	if err := reader.List(ctx, &clusters); err != nil {
		return nil, fmt.Errorf("list Kubernetes target Clusters: %w", err)
	}
	for _, target := range b.Spec.KubernetesTargets {
		if target.Template == "" || namespaces[target.Template] != "" || target.Cluster.Namespace != "" || target.Cluster == b.Spec.Cluster {
			return invalid("invalid or duplicate Kubernetes target")
		}
		var cluster native.Cluster
		if err := reader.Get(ctx, client.ObjectKey{Name: target.Cluster.Name}, &cluster); err != nil {
			return nil, fmt.Errorf("read Kubernetes target Cluster: %w", err)
		}
		if err := ownership.MatchesNative(target.Cluster, &cluster); err != nil {
			return invalid("Kubernetes target Cluster instance differs")
		}
		if tenant.IDOf(&cluster) != b.Spec.TenantID || cluster.Spec.AgentNamespace == "" || cluster.Spec.AgentNamespace == b.Namespace || !cluster.DeletionTimestamp.IsZero() || cluster.Spec.Cordoned || cluster.Spec.Draining {
			return invalid("Kubernetes target tenant, lifecycle or namespace conflicts")
		}
		for _, other := range clusters.Items {
			if other.UID != cluster.UID && other.Spec.AgentNamespace == cluster.Spec.AgentNamespace {
				return invalid("Kubernetes target namespace is claimed by another Cluster")
			}
		}
		var companions ext.ContainerClusterList
		if err := reader.List(ctx, &companions, client.InNamespace(cluster.Spec.AgentNamespace)); err != nil {
			return nil, fmt.Errorf("list Kubernetes target companion Clusters: %w", err)
		}
		if len(companions.Items) != 0 {
			return invalid("Kubernetes target namespace belongs to a container Cluster")
		}
		matched := false
		for _, t := range job.Spec.Tasks {
			if t.Name == target.Template && t.Runtime.Kind == "Kubernetes" {
				matched = true
			}
		}
		if !matched {
			return invalid("Kubernetes target has no matching template")
		}
		namespaces[target.Template] = cluster.Spec.AgentNamespace
	}
	for _, t := range job.Spec.Tasks {
		if t.Runtime.Kind == "Kubernetes" && namespaces[t.Name] == "" {
			return invalid("Kubernetes template target is absent")
		}
	}
	return namespaces, nil
}
