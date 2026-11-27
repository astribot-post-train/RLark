package clusterupgrade

import (
	"context"
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

type fakeNodeImages struct {
	image, failImage string
}

func (f *fakeNodeImages) Read(context.Context) (NodeAgentTarget, error) {
	return NodeAgentTarget{ClusterUID: "cluster-uid", Before: f.image}, nil
}
func (f *fakeNodeImages) CompareAndSet(_ context.Context, target NodeAgentTarget, old, next string, _ bool) error {
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
	if image == f.failImage {
		return errors.New("node rollout could not become ready")
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
		if err := executor.Run(t.Context(), target, "next-node-agent", true); !errors.Is(err, ErrRolledBack) {
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

func TestExecutorKeepsNodeAgentImageIndependent(t *testing.T) {
	target := Images{Agent: "cluster-target", Tools: "tools-target"}
	const nodeTarget = "node-target"
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-agent", Namespace: Namespace, UID: "agent-uid"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "agent", Image: "old-cluster", Args: []string{"--mode=cluster", "--image=old-tools"}}},
		}}},
		Status: appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1},
	}
	client := fake.NewClientset(deployment, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: CheckpointConfigMap, Namespace: Namespace},
	})
	checks := 0
	held := func(context.Context) error {
		checks++
		if checks == 4 {
			return apierrors.NewServiceUnavailable("member API restarting during readiness check")
		}
		return nil
	}
	store := ConfigMapCheckpoint{ConfigMaps: client.CoreV1().ConfigMaps(Namespace), JobUID: "job-two", CheckLease: held}
	nodes := &fakeNodeImages{image: "old-node"}
	executor := Executor{
		Deployments: client.AppsV1().Deployments(Namespace), NodeAgents: nodes, Checkpoints: store,
		CheckLease: held, HealthTimeout: 3 * time.Second,
	}
	if err := executor.Run(t.Context(), target, "", true); err == nil {
		t.Fatal("a managed node Agent upgrade accepted a missing node image")
	}
	if _, found, err := store.Load(t.Context()); err != nil || found {
		t.Fatalf("invalid upgrade created a checkpoint: %v, %v", found, err)
	}
	if err := executor.Run(t.Context(), target, nodeTarget, true); err != nil {
		t.Fatal(err)
	}
	state, found, err := store.Load(t.Context())
	if err != nil || !found || !state.Completed || state.Target != target || state.NodeImage != nodeTarget || nodes.image != nodeTarget {
		t.Fatalf("unexpected result: node image %q, checkpoint %+v, found %v, error %v", nodes.image, state, found, err)
	}
	if err := executor.Run(t.Context(), target, "another-node-target", true); err == nil {
		t.Fatal("a resumed job accepted a changed node image target")
	}
	// A new attempt whose node rollout fails restores its own baseline,
	// including the node image that differs from the cluster Agent image.
	store.JobUID = "job-three"
	executor.Checkpoints = store
	nodes.failImage = "broken-node"
	broken := Images{Agent: "different-cluster", Tools: "different-tools"}
	if err := executor.Run(t.Context(), broken, nodes.failImage, true); !errors.Is(err, ErrRolledBack) {
		t.Fatalf("failed node rollout was not rolled back: %v", err)
	}
	live, err := executor.Deployments.Get(t.Context(), deployment.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if live.Spec.Template.Spec.Containers[0].Image != target.Agent || nodes.image != nodeTarget {
		t.Fatalf("wrong rollback targets: cluster image %q, node image %q", live.Spec.Template.Spec.Containers[0].Image, nodes.image)
	}
}
