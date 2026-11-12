package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	stationv1alpha1 "github.com/rlinf/rlark/extensions/station/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestResetScriptsRejectInvalidIdentityAndDoNotRepeat(t *testing.T) {
	uid := "01234567-89ab-cdef-0123-456789abcdef"
	script, err := NodeResetAttemptScript(uid)
	if err != nil || !strings.Contains(script, "set -C") || !strings.Contains(script, "kubeadm reset --force") || !strings.Contains(script, "$base.done") {
		t.Fatalf("reset script: %v", err)
	}
	if _, err := NodeResetAttemptScript("../invalid"); err == nil {
		t.Fatal("accepted invalid operation identity")
	}
	for _, output := range []string{"completed\n", "attempted\n", "configured\n-----BEGIN CERTIFICATE-----\ncert\n-----END CERTIFICATE-----\n"} {
		if _, _, err := parseNodeResetObservation([]byte(output)); err != nil {
			t.Fatalf("%q: %v", output, err)
		}
	}
	for _, output := range []string{"configured\n", "completed\nsecret", "configured\n-----BEGIN PRIVATE KEY-----"} {
		if _, _, err := parseNodeResetObservation([]byte(output)); err == nil {
			t.Fatalf("accepted %q", output)
		}
	}
}

type resetDriverStub struct{ called bool }

func (s *resetDriverStub) Reset(context.Context, *stationv1alpha1.NodeOperation) (bool, error) {
	s.called = true
	return true, nil
}

func TestResetRequiresRemovalPrerequisites(t *testing.T) {
	stub := &resetDriverStub{}
	op := &stationv1alpha1.NodeOperation{Spec: stationv1alpha1.NodeOperationSpec{Action: "Remove", NodeUID: "uid"}}
	_, _, err := (&NodeLifecycleExecutor{ResetDriver: stub}).Execute(context.Background(), op, "Reset")
	if !errors.Is(err, ErrNodeStageUnavailable) || stub.called {
		t.Fatalf("reset without prerequisites: %v", err)
	}
}

func TestStaleResetIsOptInAndRequiresEverySafetyDependency(t *testing.T) {
	op := &stationv1alpha1.NodeOperation{Spec: stationv1alpha1.NodeOperationSpec{Action: "Join", NodeName: "node-a"}}
	done, _, err := (&NodeLifecycleExecutor{}).Execute(context.Background(), op, "ResetExisting")
	if err != nil || !done {
		t.Fatalf("default Join should skip reset: done=%v err=%v", done, err)
	}
	op.Spec.ResetExistingKubeadm = true
	if _, _, err := (&NodeLifecycleExecutor{}).Execute(context.Background(), op, "ResetExisting"); !errors.Is(err, ErrNodeStageUnavailable) {
		t.Fatalf("reset without registered cluster checks should fail closed: %v", err)
	}
}

func TestStaleResetScriptsRequireMatchingNonControlPlaneHostAndDoNotReplay(t *testing.T) {
	uid := "01234567-89ab-cdef-0123-456789abcdef"
	systemUUID := "12345678-1234-1234-1234-123456789abc"
	script, err := NodeStaleResetAttemptScript(uid, "nb260706069", systemUUID)
	if err != nil || !strings.Contains(script, "kubeadm reset --force") || !strings.Contains(script, "hostname -s") || !strings.Contains(script, "kube-apiserver.yaml") || !strings.Contains(script, "$base.done") || !strings.Contains(script, "set -C") || !strings.Contains(script, systemUUID) {
		t.Fatalf("stale reset script: %v", err)
	}
	for _, output := range []string{"configured 12345678-1234-1234-1234-123456789abc\n", "clean 12345678-1234-1234-1234-123456789abc\n", "attempted 12345678-1234-1234-1234-123456789abc\n", "completed 12345678-1234-1234-1234-123456789abc\n"} {
		if _, _, err := parseNodeStaleResetObservation([]byte(output)); err != nil {
			t.Fatalf("%q: %v", output, err)
		}
	}
	for _, tc := range []struct {
		output string
		want   error
	}{{"hostname-mismatch\n", ErrStaleResetHostMismatch}, {"control-plane\n", ErrStaleResetControlPlane}, {"identity-unavailable\n", ErrStaleResetIdentityUnavailable}} {
		if _, _, err := parseNodeStaleResetObservation([]byte(tc.output)); !errors.Is(err, tc.want) {
			t.Fatalf("%q: want %v, got %v", tc.output, tc.want, err)
		}
	}
	if _, err := NodeStaleResetAttemptScript(uid, "Not a node name", systemUUID); err == nil {
		t.Fatal("accepted invalid node name")
	}
}

func TestStaleResetChecksNodeNameAndSystemUUIDInTargetCluster(t *testing.T) {
	clusterB := k8sfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "renamed-node", UID: "node-uid"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{SystemUUID: "12345678-1234-1234-1234-123456789abc"}}})
	if err := verifyHostNotRegistered(context.Background(), clusterB, "nb260706069", "12345678-1234-1234-1234-123456789abc"); err == nil || !strings.Contains(err.Error(), "system UUID is already registered") {
		t.Fatalf("expected UUID ownership protection, got %v", err)
	}
	clusterB = k8sfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "nb260706069", UID: "other-node"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{SystemUUID: "aaaaaaaa-1234-1234-1234-123456789abc"}}})
	if err := verifyHostNotRegistered(context.Background(), clusterB, "nb260706069", "12345678-1234-1234-1234-123456789abc"); err == nil || !strings.Contains(err.Error(), "already exists in target cluster") {
		t.Fatalf("target Node name collision should block reset: %v", err)
	}
}
