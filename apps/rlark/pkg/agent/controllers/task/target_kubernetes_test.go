package task

import (
	"context"
	"errors"
	"testing"

	"github.com/rlinf/rlark/api/metadata"
	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestKubernetesTargetAdmission(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*api.Task, *api.Cluster) []client.Object
		denied bool
	}{
		{name: "canonical"},
		{name: "legacy", mutate: func(task *api.Task, _ *api.Cluster) []client.Object {
			task.Annotations = map[string]string{legacyTargetClusterUID: "cluster-uid"}
			return nil
		}},
		{name: "matching aliases", mutate: func(task *api.Task, _ *api.Cluster) []client.Object {
			task.Annotations[legacyTargetClusterUID] = "cluster-uid"
			return nil
		}},
		{name: "historical unpinned", mutate: func(task *api.Task, _ *api.Cluster) []client.Object {
			task.Annotations = nil
			return nil
		}},
		{name: "empty canonical cannot bypass", denied: true, mutate: func(task *api.Task, _ *api.Cluster) []client.Object {
			task.Annotations[metadata.TargetClusterUIDAnnotation] = ""
			return nil
		}},
		{name: "conflicting aliases", denied: true, mutate: func(task *api.Task, _ *api.Cluster) []client.Object {
			task.Annotations[legacyTargetClusterUID] = "other"
			return nil
		}},
		{name: "recreated Cluster", denied: true, mutate: func(_ *api.Task, cluster *api.Cluster) []client.Object {
			cluster.UID = "replacement"
			return nil
		}},
		{name: "wrong namespace", denied: true, mutate: func(task *api.Task, _ *api.Cluster) []client.Object {
			task.Namespace = "other"
			return nil
		}},
		{name: "tenant mismatch", denied: true, mutate: func(task *api.Task, _ *api.Cluster) []client.Object {
			task.Labels[metadata.IDLabel] = "other"
			return nil
		}},
		{name: "namespace label grants no authority", denied: true, mutate: func(_ *api.Task, cluster *api.Cluster) []client.Object {
			cluster.Spec.AgentNamespace = "elsewhere"
			cluster.Labels[api.LabelClusterID] = "robot"
			return nil
		}},
		{name: "ambiguous membership", denied: true, mutate: func(_ *api.Task, cluster *api.Cluster) []client.Object {
			other := cluster.DeepCopy()
			other.Name = "other"
			other.UID = "other"
			return []client.Object{other}
		}},
		{name: "deleting Cluster", denied: true, mutate: func(_ *api.Task, cluster *api.Cluster) []client.Object {
			now := metav1.Now()
			cluster.DeletionTimestamp = &now
			cluster.Finalizers = []string{"test/finalizer"}
			return nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := api.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			task := &api.Task{ObjectMeta: metav1.ObjectMeta{Name: "env", Namespace: "robot-agents", UID: "task-uid", Labels: map[string]string{metadata.IDLabel: "tenant"}, Annotations: map[string]string{metadata.TargetClusterUIDAnnotation: "cluster-uid"}}, Spec: api.TaskSpec{Runtime: api.TaskRuntimeSpec{Kind: api.RuntimeKindKubernetes}}}
			cluster := &api.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "robot", UID: "cluster-uid", Labels: map[string]string{metadata.IDLabel: "tenant"}}, Spec: api.ClusterSpec{AgentNamespace: "robot-agents"}}
			objects := []client.Object{}
			if test.mutate != nil {
				objects = test.mutate(task, cluster)
			}
			objects = append(objects, cluster)
			management := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			err := validateKubernetesTarget(context.Background(), management, "robot-agents", task)
			if test.denied && !fault.Is(err, fault.PermissionDenied) || !test.denied && err != nil {
				t.Fatalf("admission: %v", err)
			}
		})
	}
}

type unavailableTargetReader struct {
	client.Reader
	cause error
}

func (r unavailableTargetReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return r.cause
}

func TestKubernetesTargetOutageAndRecovery(t *testing.T) {
	cause := errors.New("management unavailable")
	task := &api.Task{ObjectMeta: metav1.ObjectMeta{Name: "env", Namespace: "agents", UID: "task", Annotations: map[string]string{metadata.TargetClusterUIDAnnotation: "cluster"}}, Spec: api.TaskSpec{Runtime: api.TaskRuntimeSpec{Kind: api.RuntimeKindKubernetes}}}
	err := validateKubernetesTarget(context.Background(), unavailableTargetReader{cause: cause}, "agents", task)
	if !errors.Is(err, cause) {
		t.Fatalf("lost dependency error: %v", err)
	}
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := &api.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "robot", UID: "cluster"}, Spec: api.ClusterSpec{AgentNamespace: "agents"}}
	management := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, task).Build()
	if err := validateKubernetesTarget(context.Background(), management, "agents", task); err != nil {
		t.Fatal(err)
	}
	// Host Get must deny before it can persist a finalizer/claim.
	guarded := kubernetesTaskClient{Client: management, namespace: "other"}
	var current api.Task
	if err := guarded.Get(context.Background(), client.ObjectKeyFromObject(task), &current); !fault.Is(err, fault.PermissionDenied) {
		t.Fatalf("unfenced Task read: %v", err)
	}
	// Deletion bypasses admission so only the existing Host owner can clean its
	// UID-bound runtime, even if the target Cluster has since been replaced.
	current.Finalizers = []string{"test/finalizer"}
	if err := management.Update(context.Background(), &current); err != nil {
		t.Fatal(err)
	}
	if err := management.Delete(context.Background(), &current); err != nil {
		t.Fatal(err)
	}
	if err := guarded.Get(context.Background(), client.ObjectKeyFromObject(task), &current); err != nil {
		t.Fatal(err)
	}
}
