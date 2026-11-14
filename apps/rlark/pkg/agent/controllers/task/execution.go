package task

import (
	"sort"

	corev1 "k8s.io/api/core/v1"

	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	kubernetesruntime "github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/runtime/kubernetes"
	raykubernetes "github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/workload/ray/kubernetes"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/coordination/coordinator"
	agentruntime "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func observedRootInstanceNodes(result coordinator.ObserveResult) []string {
	return instanceNodes(result.Instances)
}

func instanceNodes(instances agentruntime.InstanceSnapshot) []string {
	nodes := make(map[string]struct{})
	for _, instance := range instances.Instances {
		for _, condition := range instance.Conditions {
			if condition.Source == "kubernetes" && condition.Scope == "node" && condition.Subject != "" {
				nodes[condition.Subject] = struct{}{}
			}
		}
	}
	resultNodes := make([]string, 0, len(nodes))
	for node := range nodes {
		resultNodes = append(resultNodes, node)
	}
	sort.Strings(resultNodes)
	return resultNodes
}

// perNodeInstances projects a per-node Task's observed Pods into its status
// instances, sorted by node. An instance is Running when its Pod runs and is
// ready, Failed when a container cannot start or keeps crashing, and Pending
// otherwise, which includes a Pod on a node that stopped reporting.
func perNodeInstances(instances agentruntime.InstanceSnapshot) []rlarkv1alpha1.TaskInstanceStatus {
	result := make([]rlarkv1alpha1.TaskInstanceStatus, 0, len(instances.Instances))
	for _, instance := range instances.Instances {
		status := rlarkv1alpha1.TaskInstanceStatus{Name: instance.Name, Phase: rlarkv1alpha1.TaskInstancePending}
		ready, waiting := false, ""
		for _, condition := range instance.Conditions {
			switch {
			case condition.Scope == "node":
				status.Node = condition.Subject
			case condition.Scope == "image" && condition.Subject == "main":
				status.Image = condition.Message
			case condition.Scope == "instance" && condition.Reason == "Ready":
				ready = true
			case condition.Scope == "container" && waiting == "":
				waiting = condition.Reason
			}
		}
		single := agentruntime.InstanceSnapshot{Instances: []agentruntime.Instance{instance}}
		if failure, found := raykubernetes.InstanceFailureMessage(single); found {
			status.Phase, status.Message = rlarkv1alpha1.TaskInstanceFailed, failure
		} else if ready && instance.Phase == agentruntime.InstancePhase(corev1.PodRunning) {
			status.Phase = rlarkv1alpha1.TaskInstanceRunning
		} else if raykubernetes.HasFailedScheduling(single) {
			status.Message = "FailedScheduling"
		} else if waiting != "" {
			status.Message = waiting
		} else if instance.Phase == agentruntime.InstancePhase(corev1.PodRunning) {
			status.Message = "not ready"
		}
		result = append(result, status)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Node != result[j].Node {
			return result[i].Node < result[j].Node
		}
		return result[i].Name < result[j].Name
	})
	return result
}

func kubernetesWorkloadRef(task *rlarkv1alpha1.Task, object client.Object, kind string) agentruntime.ObjectRef {
	return agentruntime.ObjectRef{RuntimeID: kubernetesruntime.DefaultID, ID: "apps/v1, Kind=" + kind, Namespace: object.GetNamespace(), Name: object.GetName(), UID: string(object.GetUID()), LogicalTask: agentruntime.LogicalTaskID{Namespace: task.Namespace, Name: task.Name}}
}
