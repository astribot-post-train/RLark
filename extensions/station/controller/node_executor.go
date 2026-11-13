package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"

	stationv1alpha1 "github.com/rlinf/rlark/extensions/station/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// NodeCredentialSource must verify that the referenced short-lived Secret is
// owned by this operation before returning its data.
type NodeCredentialSource interface {
	SSHCredential(context.Context, *stationv1alpha1.NodeOperation) (password, privateKey []byte, err error)
}

type NodeSudoCredentialSource interface {
	SudoCredential(context.Context, *stationv1alpha1.NodeOperation) ([]byte, error)
}

type NodeCredentialCleaner interface {
	Cleanup(context.Context, *stationv1alpha1.NodeOperation) (bool, error)
}

type NodeOperationCleanupExecutor interface {
	CleanupOperation(context.Context, *stationv1alpha1.NodeOperation) (bool, error)
}

type NodeMemberClientSource interface {
	Client(context.Context, string) (kubernetes.Interface, error)
}

// JoinedNodeReadiness verifies RLark's observation belongs to the specified
// member Node instance, rather than accepting a same-named stale record.
type JoinedNodeReadiness interface {
	Ready(context.Context, string, string, string, string) (bool, error)
}

type NodeLifecycleExecutor struct {
	Credentials       NodeCredentialSource
	Members           NodeMemberClientSource
	JoinDriver        NodeJoinDriver
	ResetDriver       NodeResetDriver
	StaleResetDriver  StaleNodeResetDriver
	RegisteredMembers NodeMemberClientSource
	PeerCleaner       NodeWireGuardPeerCleaner
	JoinedReadiness   JoinedNodeReadiness
	// RemovalReady checks that reset and peer cleanup are available before drain.
	RemovalReady func(context.Context, *stationv1alpha1.NodeOperation) error
}

var ErrNodeStageUnavailable = errors.New("node operation stage is not implemented")

// CleanupOperation only revokes operation-owned credentials; it does not
// reverse a partially completed join or removal.
func (e *NodeLifecycleExecutor) CleanupOperation(ctx context.Context, op *stationv1alpha1.NodeOperation) (bool, error) {
	if op == nil {
		return false, errors.New("operation is required")
	}
	if op.Spec.Action == "Join" {
		if e.Members == nil {
			return false, ErrNodeStageUnavailable
		}
		member, err := e.Members.Client(ctx, op.Spec.ClusterID)
		if err != nil {
			return false, fmt.Errorf("connect member cluster: %w", err)
		}
		if err := RevokeNodeBootstrapToken(ctx, member, op.UID); err != nil {
			return false, err
		}
	}
	if op.Spec.CredentialSecret == "" {
		return true, nil
	}
	cleaner, ok := e.Credentials.(NodeCredentialCleaner)
	if !ok {
		return false, ErrNodeStageUnavailable
	}
	return cleaner.Cleanup(ctx, op)
}

// Execute only completes stages whose effects are verified by current state.
// Unsupported stages fail closed until the corresponding integration exists.
func (e *NodeLifecycleExecutor) Execute(ctx context.Context, op *stationv1alpha1.NodeOperation, stage string) (bool, string, error) {
	if op == nil {
		return false, "", errors.New("operation is required")
	}
	switch stage {
	case "CleanupCredential":
		cleaner, ok := e.Credentials.(NodeCredentialCleaner)
		if !ok {
			return false, "", ErrNodeStageUnavailable
		}
		done, err := cleaner.Cleanup(ctx, op)
		return done, op.Status.NodeUID, err
	case "Join":
		return ExecuteNodeJoin(ctx, e.JoinDriver, op)
	case "ResetExisting":
		if op.Spec.Action != "Join" {
			return false, "", ErrNodeStageUnavailable
		}
		if !op.Spec.ResetExistingKubeadm {
			return true, "", nil
		}
		if e.StaleResetDriver == nil || e.RegisteredMembers == nil {
			return false, "", ErrNodeStageUnavailable
		}
		done, err := e.StaleResetDriver.ResetExisting(ctx, op, e.RegisteredMembers)
		return done, "", err
	case "Reset":
		if op.Spec.Action != "Remove" || e.ResetDriver == nil || e.PeerCleaner == nil {
			return false, "", ErrNodeStageUnavailable
		}
		done, err := e.ResetDriver.Reset(ctx, op)
		return done, op.Spec.NodeUID, err
	case "RemovePeer":
		if op.Spec.Action != "Remove" || e.PeerCleaner == nil {
			return false, "", ErrNodeStageUnavailable
		}
		done, err := e.PeerCleaner.Remove(ctx, op)
		return done, op.Spec.NodeUID, err
	case "Preflight":
		if op.Spec.Action == "EnableWireGuard" {
			return false, "", ErrNodeStageUnavailable
		}
		if op.Spec.Action == "Remove" {
			if e.RemovalReady == nil {
				return false, "", ErrNodeStageUnavailable
			}
			if err := e.RemovalReady(ctx, op); err != nil {
				return false, "", fmt.Errorf("verify removal prerequisites: %w", err)
			}
		}
		if e.Credentials == nil || op.Spec.SSH == nil {
			return false, "", errors.New("SSH credential and connection are required")
		}
		password, key, err := e.Credentials.SSHCredential(ctx, op)
		if err != nil {
			return false, "", fmt.Errorf("read operation SSH credential: %w", err)
		}
		defer clear(password)
		defer clear(key)
		connection := op.Spec.SSH
		sshClient, err := DialNodeSSH(ctx, NodeSSHConfig{Address: net.JoinHostPort(connection.Address, strconv.Itoa(int(connection.Port))), User: connection.User, HostKeyFingerprint: connection.HostKeyFingerprint, Password: password, PrivateKey: key})
		if err != nil {
			return false, "", err
		}
		defer sshClient.Close()
		_, err = RunNodeSSHCommand(ctx, sshClient, "command -v kubeadm >/dev/null && command -v kubelet >/dev/null && command -v containerd >/dev/null", nil)
		if err != nil {
			return false, "", fmt.Errorf("check node prerequisites: %w", err)
		}
		sudoSource, ok := e.Credentials.(NodeSudoCredentialSource)
		if !ok {
			return false, "", errors.New("sudo credential source is required")
		}
		sudoPassword, err := sudoSource.SudoCredential(ctx, op)
		if err != nil {
			return false, "", fmt.Errorf("read sudo credential: %w", err)
		}
		defer clear(sudoPassword)
		if _, err := RunNodeSudoScript(ctx, sshClient, "true", sudoPassword); err != nil {
			return false, "", fmt.Errorf("check sudo access: %w", err)
		}
		if op.Spec.Action == "Join" {
			socket, err := containerdSocketForRuntime(op.Spec.ContainerdRuntime)
			if err != nil {
				return false, "", err
			}
			if _, err := RunNodeSudoScript(ctx, sshClient, "test -S '"+socket+"'", sudoPassword); err != nil {
				return false, "", fmt.Errorf("containerd socket %s is unavailable: %w", socket, err)
			}
			output, err := RunNodeSudoScript(ctx, sshClient, "sysctl -n net.ipv4.ip_forward", sudoPassword)
			if err != nil {
				return false, "", fmt.Errorf("check IPv4 forwarding: %w", err)
			}
			if string(bytes.TrimSpace(output)) != "1" {
				return false, "", errors.New("net.ipv4.ip_forward must be 1 before Kubernetes Join")
			}
		}
		return true, op.Spec.NodeUID, nil
	case "Drain":
		if op.Spec.Action != "Remove" || e.Members == nil || e.RemovalReady == nil {
			return false, "", ErrNodeStageUnavailable
		}
		if err := e.RemovalReady(ctx, op); err != nil {
			return false, "", fmt.Errorf("verify removal prerequisites: %w", err)
		}
		member, err := e.Members.Client(ctx, op.Spec.ClusterID)
		if err != nil {
			return false, "", fmt.Errorf("connect member cluster: %w", err)
		}
		done, err := DrainWorker(ctx, member, op.Spec.NodeName, types.UID(op.Spec.NodeUID))
		return done, op.Spec.NodeUID, err
	case "VerifyNode":
		if op.Spec.Action != "Join" || e.Members == nil || op.Spec.NodeName == "" || op.Status.NodeUID == "" {
			return false, "", ErrNodeStageUnavailable
		}
		member, err := e.Members.Client(ctx, op.Spec.ClusterID)
		if err != nil {
			return false, "", fmt.Errorf("connect member cluster: %w", err)
		}
		node, err := member.CoreV1().Nodes().Get(ctx, op.Spec.NodeName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, op.Status.NodeUID, nil
		}
		if err != nil {
			return false, "", fmt.Errorf("read joined node: %w", err)
		}
		if string(node.UID) != op.Status.NodeUID || node.DeletionTimestamp != nil {
			return false, "", ErrNodeIdentityChanged
		}
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
				return true, op.Status.NodeUID, nil
			}
		}
		return false, op.Status.NodeUID, nil
	case "VerifyRLark":
		if op.Spec.Action != "Join" || e.Members == nil || e.JoinedReadiness == nil || op.Spec.NodeName == "" || op.Status.NodeUID == "" {
			return false, "", ErrNodeStageUnavailable
		}
		member, err := e.Members.Client(ctx, op.Spec.ClusterID)
		if err != nil {
			return false, "", fmt.Errorf("connect member cluster: %w", err)
		}
		node, err := member.CoreV1().Nodes().Get(ctx, op.Spec.NodeName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, "", ErrNodeIdentityChanged
		}
		if err != nil {
			return false, "", fmt.Errorf("read joined node: %w", err)
		}
		if string(node.UID) != op.Status.NodeUID || node.DeletionTimestamp != nil {
			return false, "", ErrNodeIdentityChanged
		}
		ready, err := e.JoinedReadiness.Ready(ctx, op.Spec.ClusterID, op.Namespace, op.Spec.NodeName, op.Status.NodeUID)
		if err != nil {
			return false, "", fmt.Errorf("verify RLark node: %w", err)
		}
		return ready, op.Status.NodeUID, nil
	case "ConfigureAgentRuntime":
		if op.Spec.Action != "Join" || e.Members == nil || op.Spec.NodeName == "" || op.Status.NodeUID == "" {
			return false, "", ErrNodeStageUnavailable
		}
		member, err := e.Members.Client(ctx, op.Spec.ClusterID)
		if err != nil {
			return false, "", fmt.Errorf("connect member cluster: %w", err)
		}
		node, err := member.CoreV1().Nodes().Get(ctx, op.Spec.NodeName, metav1.GetOptions{})
		if err != nil {
			return false, "", fmt.Errorf("read joined Node before runtime labeling: %w", err)
		}
		if string(node.UID) != op.Status.NodeUID || node.DeletionTimestamp != nil {
			return false, "", ErrNodeIdentityChanged
		}
		runtime := op.Spec.ContainerdRuntime
		if runtime == "" {
			runtime = "containerd"
		}
		if _, err := containerdSocketForRuntime(runtime); err != nil {
			return false, "", err
		}
		if value := node.Labels["rlark.io/containerd"]; value != "" && value != runtime {
			return false, "", errors.New("joined Node has a conflicting containerd runtime label")
		}
		if value := node.Labels["rlark.io/cluster-id"]; value != "" && value != op.Spec.ClusterID {
			return false, "", errors.New("joined Node has a conflicting RLark cluster label")
		}
		if value := node.Labels["rlark.io/enabled"]; value != "" && value != "true" {
			return false, "", errors.New("joined Node has a conflicting RLark enablement label")
		}
		if node.Labels["rlark.io/containerd"] == runtime && node.Labels["rlark.io/cluster-id"] == op.Spec.ClusterID && node.Labels["rlark.io/enabled"] == "true" {
			return true, op.Status.NodeUID, nil
		}
		updated := node.DeepCopy()
		if updated.Labels == nil {
			updated.Labels = make(map[string]string)
		}
		updated.Labels["rlark.io/containerd"] = runtime
		updated.Labels["rlark.io/cluster-id"] = op.Spec.ClusterID
		updated.Labels["rlark.io/enabled"] = "true"
		if _, err := member.CoreV1().Nodes().Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
			return false, "", fmt.Errorf("configure RLark node-agent runtime: %w", err)
		}
		return true, op.Status.NodeUID, nil
	case "DeleteNode", "VerifyRemoval":
		if op.Spec.Action != "Remove" || e.Members == nil || op.Spec.NodeName == "" || op.Spec.NodeUID == "" {
			return false, "", ErrNodeStageUnavailable
		}
		member, err := e.Members.Client(ctx, op.Spec.ClusterID)
		if err != nil {
			return false, "", fmt.Errorf("connect member cluster: %w", err)
		}
		node, err := member.CoreV1().Nodes().Get(ctx, op.Spec.NodeName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, op.Spec.NodeUID, nil
		}
		if err != nil {
			return false, "", fmt.Errorf("read node before removal: %w", err)
		}
		if string(node.UID) != op.Spec.NodeUID {
			return false, "", ErrNodeIdentityChanged
		}
		if stage == "VerifyRemoval" || node.DeletionTimestamp != nil {
			return false, op.Spec.NodeUID, nil
		}
		uid := types.UID(op.Spec.NodeUID)
		if err := member.CoreV1().Nodes().Delete(ctx, op.Spec.NodeName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
			return false, "", fmt.Errorf("delete node: %w", err)
		}
		return false, op.Spec.NodeUID, nil
	default:
		return false, "", fmt.Errorf("%w: %s", ErrNodeStageUnavailable, stage)
	}
}
