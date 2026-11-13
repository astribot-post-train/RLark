package controller

import (
	"context"
	"errors"
	"testing"

	stationv1alpha1 "github.com/rlinf/rlark/extensions/station/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

type memberClientStub struct {
	client  kubernetes.Interface
	cluster string
}

type joinedReadinessStub struct {
	ready   bool
	err     error
	nodeUID string
}

func (s *joinedReadinessStub) Ready(_ context.Context, _, _, _, uid string) (bool, error) {
	s.nodeUID = uid
	return s.ready, s.err
}

func (s *memberClientStub) Client(_ context.Context, cluster string) (kubernetes.Interface, error) {
	s.cluster = cluster
	return s.client, nil
}

func TestNodeLifecycleExecutorDrainAndUnsupportedStages(t *testing.T) {
	member := &memberClientStub{client: fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "uid"}})}
	executor := &NodeLifecycleExecutor{Members: member, RemovalReady: func(context.Context, *stationv1alpha1.NodeOperation) error { return nil }}
	op := &stationv1alpha1.NodeOperation{Spec: stationv1alpha1.NodeOperationSpec{Action: "Remove", ClusterID: "cluster", NodeName: "worker", NodeUID: "uid"}}
	done, uid, err := executor.Execute(context.Background(), op, "Drain")
	if err != nil || !done || uid != "uid" || member.cluster != "cluster" {
		t.Fatalf("drain: %v %q %v", done, uid, err)
	}
	if _, _, err := executor.Execute(context.Background(), op, "Reset"); !errors.Is(err, ErrNodeStageUnavailable) {
		t.Fatalf("reset must fail closed: %v", err)
	}
}

func TestNodeLifecycleExecutorDoesNotDrainWithoutCleanup(t *testing.T) {
	ctx := context.Background()
	member := &memberClientStub{client: fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "uid"}})}
	op := &stationv1alpha1.NodeOperation{Spec: stationv1alpha1.NodeOperationSpec{Action: "Remove", ClusterID: "cluster", NodeName: "worker", NodeUID: "uid"}}
	if _, _, err := (&NodeLifecycleExecutor{Members: member}).Execute(ctx, op, "Drain"); !errors.Is(err, ErrNodeStageUnavailable) {
		t.Fatalf("missing cleanup: %v", err)
	}
	node, err := member.client.CoreV1().Nodes().Get(ctx, "worker", metav1.GetOptions{})
	if err != nil || node.Spec.Unschedulable {
		t.Fatalf("node was cordoned: %+v, %v", node, err)
	}
}

func TestNodeLifecycleExecutorVerifyJoinedNode(t *testing.T) {
	ctx := context.Background()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "uid"}}
	member := &memberClientStub{client: fake.NewSimpleClientset(node)}
	executor := &NodeLifecycleExecutor{Members: member}
	op := &stationv1alpha1.NodeOperation{Spec: stationv1alpha1.NodeOperationSpec{Action: "Join", ClusterID: "cluster", NodeName: "worker"}, Status: stationv1alpha1.NodeOperationStatus{NodeUID: "uid"}}
	if done, _, err := executor.Execute(ctx, op, "VerifyNode"); err != nil || done {
		t.Fatalf("unready: %v %v", done, err)
	}
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	if _, err := member.client.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if done, _, err := executor.Execute(ctx, op, "VerifyNode"); err != nil || !done {
		t.Fatalf("ready: %v %v", done, err)
	}
	op.Status.NodeUID = "old"
	if _, _, err := executor.Execute(ctx, op, "VerifyNode"); !errors.Is(err, ErrNodeIdentityChanged) {
		t.Fatalf("replaced: %v", err)
	}
}

func TestNodeLifecycleExecutorVerifyRLarkRequiresCurrentNodeIdentity(t *testing.T) {
	ctx := context.Background()
	member := &memberClientStub{client: fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "uid", Labels: map[string]string{"rlark.io/enabled": "true", "rlark.io/containerd": "containerd", "rlark.io/cluster-id": "cluster"}}})}
	readiness := &joinedReadinessStub{}
	executor := &NodeLifecycleExecutor{Members: member, JoinedReadiness: readiness}
	op := &stationv1alpha1.NodeOperation{ObjectMeta: metav1.ObjectMeta{Namespace: "agent"}, Spec: stationv1alpha1.NodeOperationSpec{Action: "Join", ClusterID: "cluster", NodeName: "worker"}, Status: stationv1alpha1.NodeOperationStatus{NodeUID: "uid"}}
	if done, _, err := executor.Execute(ctx, op, "VerifyRLark"); err != nil || done || readiness.nodeUID != "uid" {
		t.Fatalf("pending: done=%v err=%v uid=%q", done, err, readiness.nodeUID)
	}
	readiness.ready = true
	if done, uid, err := executor.Execute(ctx, op, "VerifyRLark"); err != nil || !done || uid != "uid" {
		t.Fatalf("ready: done=%v uid=%q err=%v", done, uid, err)
	}
	op.Status.NodeUID = "old"
	readiness.nodeUID = ""
	if _, _, err := executor.Execute(ctx, op, "VerifyRLark"); !errors.Is(err, ErrNodeIdentityChanged) || readiness.nodeUID != "" {
		t.Fatalf("replaced: %v", err)
	}
}

func TestNodeLifecycleExecutorDoesNotOverrideDisabledNode(t *testing.T) {
	ctx := context.Background()
	member := &memberClientStub{client: fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "uid", Labels: map[string]string{"rlark.io/enabled": "false"}}})}
	readiness := &joinedReadinessStub{ready: true}
	op := &stationv1alpha1.NodeOperation{ObjectMeta: metav1.ObjectMeta{Namespace: "agent"}, Spec: stationv1alpha1.NodeOperationSpec{Action: "Join", ClusterID: "cluster", NodeName: "worker"}, Status: stationv1alpha1.NodeOperationStatus{NodeUID: "uid"}}
	if _, _, err := (&NodeLifecycleExecutor{Members: member, JoinedReadiness: readiness}).Execute(ctx, op, "ConfigureAgentRuntime"); err == nil {
		t.Fatal("overrode disabled label")
	}
	node, err := member.client.CoreV1().Nodes().Get(ctx, "worker", metav1.GetOptions{})
	if err != nil || node.Labels["rlark.io/enabled"] != "false" || readiness.nodeUID != "" {
		t.Fatalf("label changed or readiness called: %+v %v", node, err)
	}
}

func TestConfigureAgentRuntimeLabelsExactNodeAndRejectsConflicts(t *testing.T) {
	ctx := context.Background()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "uid"}}
	member := &memberClientStub{client: fake.NewSimpleClientset(node)}
	op := &stationv1alpha1.NodeOperation{Spec: stationv1alpha1.NodeOperationSpec{Action: "Join", ClusterID: "cluster", NodeName: "worker", ContainerdRuntime: "containerd"}, Status: stationv1alpha1.NodeOperationStatus{NodeUID: "uid"}}
	executor := &NodeLifecycleExecutor{Members: member}
	done, uid, err := executor.Execute(ctx, op, "ConfigureAgentRuntime")
	if err != nil || !done || uid != "uid" {
		t.Fatalf("configure runtime: done=%v uid=%q err=%v", done, uid, err)
	}
	stored, err := member.client.CoreV1().Nodes().Get(ctx, "worker", metav1.GetOptions{})
	if err != nil || stored.Labels[nodeContainerdLabel] != "containerd" || stored.Labels[nodeClusterIDLabel] != "cluster" || stored.Labels[nodeEnabledLabel] != "true" {
		t.Fatalf("runtime labels not reconciled: labels=%v err=%v", stored.Labels, err)
	}
	stored.Labels[nodeContainerdLabel] = "fleet"
	if _, err := member.client.CoreV1().Nodes().Update(ctx, stored, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := executor.Execute(ctx, op, "ConfigureAgentRuntime"); err == nil {
		t.Fatal("overwrote conflicting runtime label")
	}
}

func TestNodeLifecycleExecutorDeleteWaitsAndRejectsReplacement(t *testing.T) {
	ctx := context.Background()
	member := &memberClientStub{client: fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "uid"}})}
	executor := &NodeLifecycleExecutor{Members: member}
	op := &stationv1alpha1.NodeOperation{Spec: stationv1alpha1.NodeOperationSpec{Action: "Remove", ClusterID: "cluster", NodeName: "worker", NodeUID: "uid"}}
	if done, _, err := executor.Execute(ctx, op, "DeleteNode"); err != nil || done {
		t.Fatalf("delete must wait: done=%v err=%v", done, err)
	}
	if done, _, err := executor.Execute(ctx, op, "DeleteNode"); err != nil || !done {
		t.Fatalf("deleted node: done=%v err=%v", done, err)
	}
	if _, err := member.client.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "new"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := executor.Execute(ctx, op, "VerifyRemoval"); !errors.Is(err, ErrNodeIdentityChanged) {
		t.Fatalf("replacement: %v", err)
	}
}
