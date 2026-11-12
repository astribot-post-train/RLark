package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	stationv1alpha1 "github.com/rlinf/rlark/extensions/station/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SSHNodeResetDriver verifies the member Node and remote kubelet identity before
// executing kubeadm reset. It is not installed until peer cleanup is available.
type SSHNodeResetDriver struct{ Connection *SSHNodeJoinDriver }

func (d SSHNodeResetDriver) Reset(ctx context.Context, op *stationv1alpha1.NodeOperation) (bool, error) {
	if d.Connection == nil || d.Connection.Members == nil || d.Connection.Kubeconfigs == nil || op == nil || op.Spec.Action != "Remove" || op.Spec.NodeUID == "" || op.Spec.NodeName == "" {
		return false, errors.New("reset operation identity is required")
	}
	member, err := d.Connection.Members.Client(ctx, op.Spec.ClusterID)
	if err != nil {
		return false, fmt.Errorf("connect member cluster: %w", err)
	}
	node, err := member.CoreV1().Nodes().Get(ctx, op.Spec.NodeName, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("read member node before reset: %w", err)
	}
	if string(node.UID) != op.Spec.NodeUID || node.DeletionTimestamp != nil || !node.Spec.Unschedulable {
		return false, ErrNodeIdentityChanged
	}
	if _, found := node.Labels["node-role.kubernetes.io/control-plane"]; found {
		return false, ErrNodeDrainBlocked
	}
	if _, found := node.Labels["node-role.kubernetes.io/master"]; found {
		return false, ErrNodeDrainBlocked
	}
	client, sudoPassword, err := d.Connection.connect(ctx, op)
	if err != nil {
		return false, err
	}
	defer client.Close()
	defer clear(sudoPassword)
	script, err := NodeResetObservationScript(string(op.UID))
	if err != nil {
		return false, err
	}
	output, err := RunNodeSudoScript(ctx, client, script, sudoPassword)
	if err != nil {
		return false, fmt.Errorf("observe remote node: %w", err)
	}
	state, cert, err := parseNodeResetObservation(output)
	if err != nil {
		return false, err
	}
	if state == "completed" {
		return true, nil
	}
	if state == "attempted" {
		return false, errors.New("reset outcome is unknown; inspect remote node before retry")
	}
	kubeconfig, err := d.Connection.Kubeconfigs.Kubeconfig(ctx, op.Spec.ClusterID)
	if err != nil {
		return false, err
	}
	config, err := secureRESTConfigFromKubeconfig(kubeconfig)
	if err != nil {
		return false, err
	}
	now := time.Now()
	if d.Connection.Now != nil {
		now = d.Connection.Now()
	}
	if _, err := VerifyKubeletIdentity(cert, config.CAData, op.Spec.NodeName, now); err != nil {
		return false, err
	}
	reset, err := NodeResetAttemptScript(string(op.UID))
	if err != nil {
		return false, err
	}
	if _, err := RunNodeSudoScript(ctx, client, reset, sudoPassword); err != nil {
		return false, fmt.Errorf("run kubeadm reset: %w", err)
	}
	return false, nil
}

type NodeResetDriver interface {
	Reset(context.Context, *stationv1alpha1.NodeOperation) (bool, error)
}
