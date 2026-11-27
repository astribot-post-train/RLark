package clusterupgrade

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestDeploymentImagesPreserveTemplateAndFenceRollback(t *testing.T) {
	ctx := t.Context()
	agent := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "site-agent", Namespace: Namespace, UID: "agent-uid"},
		Spec: appsv1.DeploymentSpec{
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				HostNetwork: true, ServiceAccountName: "site-agent",
				Containers: []corev1.Container{
					{Name: "agent", Image: "old-agent", Args: []string{"--mode=cluster", "--image", "old-tools", "--astribot-option=value"}, ReadinessProbe: &corev1.Probe{PeriodSeconds: 5}},
					{Name: "monitor", Image: "monitor"},
				},
			}},
		},
	}
	deployments := fake.NewClientset(agent).AppsV1().Deployments(Namespace)
	target, err := ReadDeploymentTarget(ctx, deployments)
	if err != nil {
		t.Fatal(err)
	}
	next := Images{"next-agent", "next-tools"}
	lost := func(context.Context) error { return errors.New("lease lost") }
	if err := WriteDeploymentImages(ctx, deployments, target, target.Before, next, lost); err == nil {
		t.Fatal("write without a lease succeeded")
	}
	held := func(context.Context) error { return nil }
	if err := WriteDeploymentImages(ctx, deployments, target, target.Before, next, held); err != nil {
		t.Fatal(err)
	}
	got, err := deployments.Get(ctx, agent.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := agent.DeepCopy()
	want.Spec.Template.Spec.Containers[0].Image = "next-agent"
	want.Spec.Template.Spec.Containers[0].Args[2] = "next-tools"
	if !apiequality.Semantic.DeepEqual(got.Spec, want.Spec) {
		t.Fatal("upgrade changed fields outside the Agent and tools images")
	}
	if err := WriteDeploymentImages(ctx, deployments, target, next, target.Before, held); err != nil {
		t.Fatal(err)
	}
	got, err = deployments.Get(ctx, agent.Name, metav1.GetOptions{})
	if err != nil || !apiequality.Semantic.DeepEqual(got.Spec, agent.Spec) {
		t.Fatalf("rollback did not restore the original spec: %v", err)
	}
	got.Spec.Template.Spec.Containers[0].Image = "concurrent-update"
	if _, err := deployments.Update(ctx, got, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := WriteDeploymentImages(ctx, deployments, target, next, target.Before, held); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("rollback accepted a third version: %v", err)
	}
}
