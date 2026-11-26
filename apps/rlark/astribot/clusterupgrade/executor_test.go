package clusterupgrade

import (
	"context"
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

type fakeNodeImages struct {
	image string
}

func (f *fakeNodeImages) Read(context.Context) (NodeAgentTarget, error) {
	return NodeAgentTarget{ClusterUID: "cluster-uid", Before: f.image}, nil
}
func (f *fakeNodeImages) CompareAndSet(_ context.Context, target NodeAgentTarget, old, next string) error {
	if target.ClusterUID != "cluster-uid" || (f.image != old && f.image != next) {
		return ErrTargetChanged
	}
	f.image = next
	return nil
}
func (f *fakeNodeImages) WaitReady(_ context.Context, _ NodeAgentTarget, image string) error {
	if f.image != image {
		return ErrTargetChanged
	}
	return nil
}

func TestFirstUpgradeCanRollBackWithoutOldUpgradeState(t *testing.T) {
	old := Images{Agent: "previous-agent", Tools: "previous-tools"}
	target := Images{Agent: "broken-agent", Tools: "next-tools"}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-agent", Namespace: Namespace, UID: "agent-uid"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "agent", Image: old.Agent, Args: []string{"--mode=cluster", "--image=" + old.Tools}}},
		}}},
		Status: appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1},
	}
	client := fake.NewClientset(deployment, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: CheckpointConfigMap, Namespace: Namespace},
	})
	client.PrependReactor("update", "deployments", func(action ktesting.Action) (bool, runtime.Object, error) {
		object := action.(ktesting.UpdateAction).GetObject().(*appsv1.Deployment)
		if object.Spec.Template.Spec.Containers[0].Image == target.Agent {
			object.Status.ReadyReplicas = 0
		} else {
			object.Status.ReadyReplicas = 1
		}
		return false, nil, nil
	})
	held := func(context.Context) error { return nil }
	store := ConfigMapCheckpoint{ConfigMaps: client.CoreV1().ConfigMaps(Namespace), JobUID: "job-one", CheckLease: held}
	nodes := &fakeNodeImages{image: "previous-node-agent"}
	executor := Executor{
		Deployments: client.AppsV1().Deployments(Namespace), NodeAgents: nodes, Checkpoints: store,
		CheckLease: held, HealthTimeout: 5 * time.Millisecond,
	}
	for i := 0; i < 2; i++ {
		if err := executor.Run(t.Context(), target, true); !errors.Is(err, ErrRolledBack) {
			t.Fatalf("attempt %d: %v", i, err)
		}
		got, err := executor.Deployments.Get(t.Context(), deployment.Name, metav1.GetOptions{})
		if err != nil || got.Spec.Template.Spec.Containers[0].Image != old.Agent || nodes.image != "previous-node-agent" {
			t.Fatalf("rollback did not restore original state: %v, node image %q", err, nodes.image)
		}
		checkpoint, saved, err := store.Load(t.Context())
		if err != nil || !saved || !checkpoint.RollingBack || checkpoint.Deployment.Before != old {
			t.Fatalf("rollback checkpoint lost: %v", err)
		}
	}
}
