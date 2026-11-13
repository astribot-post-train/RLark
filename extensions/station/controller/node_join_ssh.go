package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	stationv1alpha1 "github.com/rlinf/rlark/extensions/station/api/v1alpha1"
	"golang.org/x/crypto/ssh"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SSHNodeJoinDriver connects the join decision to an authenticated host and
// the registered member API. The manager wires it only behind the experimental
// node-operation feature flag.
type SSHNodeJoinDriver struct {
	Credentials NodeCredentialSource
	Sudo        NodeSudoCredentialSource
	Members     NodeMemberClientSource
	Kubeconfigs MemberKubeconfigSource
	Now         func() time.Time
}

func (d *SSHNodeJoinDriver) connect(ctx context.Context, op *stationv1alpha1.NodeOperation) (*ssh.Client, []byte, error) {
	if d == nil || d.Credentials == nil || d.Sudo == nil || d.Members == nil || d.Kubeconfigs == nil || op == nil || op.Spec.SSH == nil {
		return nil, nil, errors.New("join driver dependencies are required")
	}
	password, key, err := d.Credentials.SSHCredential(ctx, op)
	if err != nil {
		return nil, nil, fmt.Errorf("read SSH credential: %w", err)
	}
	defer clear(password)
	defer clear(key)
	sudoPassword, err := d.Sudo.SudoCredential(ctx, op)
	if err != nil {
		return nil, nil, fmt.Errorf("read sudo credential: %w", err)
	}
	connection := op.Spec.SSH
	client, err := DialNodeSSH(ctx, NodeSSHConfig{Address: net.JoinHostPort(connection.Address, strconv.Itoa(int(connection.Port))), User: connection.User, HostKeyFingerprint: connection.HostKeyFingerprint, Password: password, PrivateKey: key})
	if err != nil {
		clear(sudoPassword)
		return nil, nil, err
	}
	return client, sudoPassword, nil
}

func (d *SSHNodeJoinDriver) Observe(ctx context.Context, op *stationv1alpha1.NodeOperation) (NodeJoinState, error) {
	client, sudoPassword, err := d.connect(ctx, op)
	if err != nil {
		return NodeJoinState{}, err
	}
	defer client.Close()
	defer clear(sudoPassword)
	script, err := NodeJoinObservationScript(string(op.UID))
	if err != nil {
		return NodeJoinState{}, err
	}
	output, err := RunNodeSudoScript(ctx, client, script, sudoPassword)
	if err != nil {
		return NodeJoinState{}, fmt.Errorf("read remote join observation: %w", err)
	}
	state, cert, err := parseNodeJoinObservation(output)
	if err != nil {
		return NodeJoinState{}, err
	}
	if state.Attempted && len(cert) == 0 {
		state.Pending = true
	}
	if !state.Attempted || len(cert) == 0 {
		return state, nil
	}
	kubeconfig, err := d.Kubeconfigs.Kubeconfig(ctx, op.Spec.ClusterID)
	if err != nil {
		return NodeJoinState{}, fmt.Errorf("read member kubeconfig: %w", err)
	}
	config, err := secureRESTConfigFromKubeconfig(kubeconfig)
	if err != nil {
		return NodeJoinState{}, err
	}
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	if _, err := VerifyKubeletIdentity(cert, config.CAData, op.Spec.NodeName, now); err != nil {
		return NodeJoinState{}, err
	}
	member, err := d.Members.Client(ctx, op.Spec.ClusterID)
	if err != nil {
		return NodeJoinState{}, err
	}
	owned, err := VerifyJoinCSR(ctx, member, op.UID, cert)
	if err != nil {
		return NodeJoinState{}, err
	}
	if !owned {
		return NodeJoinState{}, errors.New("kubelet certificate is not linked to this join operation")
	}
	node, err := member.CoreV1().Nodes().Get(ctx, op.Spec.NodeName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		state.Pending = true
		return state, nil
	}
	if err != nil {
		return NodeJoinState{}, fmt.Errorf("read member node: %w", err)
	}
	if node.UID == "" || node.DeletionTimestamp != nil {
		return NodeJoinState{}, ErrNodeIdentityChanged
	}
	if err := RevokeNodeBootstrapToken(ctx, member, op.UID); err != nil {
		return NodeJoinState{}, fmt.Errorf("revoke node bootstrap credential: %w", err)
	}
	state.VerifiedUID = string(node.UID)
	return state, nil
}

func (d *SSHNodeJoinDriver) Start(ctx context.Context, op *stationv1alpha1.NodeOperation) error {
	if d == nil || d.Credentials == nil || d.Sudo == nil || d.Members == nil || d.Kubeconfigs == nil || op == nil || op.Spec.SSH == nil {
		return errors.New("join driver dependencies are required")
	}
	member, err := d.Members.Client(ctx, op.Spec.ClusterID)
	if err != nil {
		return err
	}
	if _, err := member.CoreV1().Nodes().Get(ctx, op.Spec.NodeName, metav1.GetOptions{}); err == nil {
		return ErrNodeAlreadyConfigured
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("check member node: %w", err)
	}
	kubeconfig, err := d.Kubeconfigs.Kubeconfig(ctx, op.Spec.ClusterID)
	if err != nil {
		return err
	}
	discovery, err := ReadNodeJoinDiscovery(ctx, member, kubeconfig)
	if err != nil {
		return err
	}
	discovery.CRISocket, err = containerdSocketForRuntime(op.Spec.ContainerdRuntime)
	if err != nil {
		return err
	}
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	token, err := EnsureNodeBootstrapToken(ctx, member, op.UID, now)
	if err != nil {
		return err
	}
	script, err := NodeJoinAttemptScript(string(op.UID), op.Spec.NodeName, discovery, token)
	if err != nil {
		return err
	}
	client, sudoPassword, err := d.connect(ctx, op)
	if err != nil {
		return err
	}
	defer client.Close()
	defer clear(sudoPassword)
	_, err = RunNodeSudoScript(ctx, client, script, sudoPassword)
	if err != nil {
		return fmt.Errorf("run kubeadm join: %w", err)
	}
	return nil
}
