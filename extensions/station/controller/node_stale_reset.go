package controller

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	stationv1alpha1 "github.com/rlinf/rlark/extensions/station/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
)

var (
	ErrStaleResetHostMismatch        = errors.New("SSH host name does not match requested Node name")
	ErrStaleResetControlPlane        = errors.New("stale kubeadm cleanup is forbidden on a control-plane host")
	ErrStaleResetAttemptUnknown      = errors.New("stale kubeadm reset outcome is unknown; inspect the host before retrying")
	ErrStaleResetIdentityUnavailable = errors.New("stable host identity is unavailable")
)

var systemUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type StaleNodeResetDriver interface {
	ResetExisting(context.Context, *stationv1alpha1.NodeOperation, NodeMemberClientSource) (bool, error)
}

// SSHStaleNodeResetDriver resets only an explicitly approved, unregistered
// worker host and never replays a reset whose outcome cannot be observed.
type SSHStaleNodeResetDriver struct{ Connection *SSHNodeJoinDriver }

func (d *SSHStaleNodeResetDriver) ResetExisting(ctx context.Context, op *stationv1alpha1.NodeOperation, members NodeMemberClientSource) (bool, error) {
	if d == nil || d.Connection == nil || op == nil || op.UID == "" || op.Spec.Action != "Join" || !op.Spec.ResetExistingKubeadm || members == nil {
		return false, errors.New("explicit stale kubeadm reset dependencies are required")
	}
	member, err := members.Client(ctx, op.Spec.ClusterID)
	if err != nil {
		return false, fmt.Errorf("read target cluster before stale reset: %w", err)
	}
	client, sudoPassword, err := d.Connection.connect(ctx, op)
	if err != nil {
		return false, err
	}
	defer client.Close()
	defer clear(sudoPassword)

	observe, err := NodeStaleResetObservationScript(string(op.UID), op.Spec.NodeName)
	if err != nil {
		return false, err
	}
	output, err := RunNodeSudoScript(ctx, client, observe, sudoPassword)
	if err != nil {
		return false, fmt.Errorf("observe stale kubeadm state: %w", err)
	}
	state, hostSystemUUID, err := parseNodeStaleResetObservation(output)
	if err != nil {
		return false, err
	}
	if err := verifyHostNotRegistered(ctx, member, op.Spec.NodeName, hostSystemUUID); err != nil {
		return false, err
	}
	if state == "attempted" {
		return false, ErrStaleResetAttemptUnknown
	}
	if state == "clean" || state == "completed" {
		return true, nil
	}

	if state != "configured" {
		return false, errors.New("stale kubeadm state changed during verification")
	}

	reset, err := NodeStaleResetAttemptScript(string(op.UID), op.Spec.NodeName, hostSystemUUID)
	if err != nil {
		return false, err
	}
	if _, err := RunNodeSudoScript(ctx, client, reset, sudoPassword); err != nil {
		return false, fmt.Errorf("reset stale kubeadm state: %w", err)
	}
	output, err = RunNodeSudoScript(ctx, client, observe, sudoPassword)
	if err != nil {
		return false, fmt.Errorf("verify stale kubeadm reset: %w", err)
	}
	state, _, err = parseNodeStaleResetObservation(output)
	if err != nil {
		return false, err
	}
	if state != "completed" {
		return false, errors.New("stale kubeadm reset was not verified")
	}
	return true, nil
}

func verifyHostNotRegistered(ctx context.Context, member kubernetes.Interface, nodeName, hostSystemUUID string) error {
	if member == nil || nodeName == "" {
		return errors.New("target cluster identity checks are required")
	}
	hostSystemUUID = normalizeSystemUUID(hostSystemUUID)
	if hostSystemUUID == "" {
		return ErrStaleResetIdentityUnavailable
	}
	_, err := member.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err == nil {
		return fmt.Errorf("Node %q already exists in target cluster; use Remove", nodeName)
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("verify target Node %q is absent: %w", nodeName, err)
	}
	nodes, err := member.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list Nodes in target cluster before stale reset: %w", err)
	}
	for i := range nodes.Items {
		if registeredUUID := normalizeSystemUUID(nodes.Items[i].Status.NodeInfo.SystemUUID); registeredUUID != "" && registeredUUID == hostSystemUUID {
			return fmt.Errorf("host system UUID is already registered by Node %q in target cluster", nodes.Items[i].Name)
		}
	}
	return nil
}

func NodeStaleResetObservationScript(operationUID, nodeName string) (string, error) {
	if err := validateNodeOperationUID(operationUID); err != nil {
		return "", err
	}
	if len(validation.IsDNS1123Subdomain(nodeName)) != 0 {
		return "", errors.New("invalid Kubernetes node name")
	}
	return fmt.Sprintf("set -eu\nexpected='%s'\nactual=$(hostname -s | tr '[:upper:]' '[:lower:]')\n[ \"$actual\" = \"$expected\" ] || { printf 'hostname-mismatch\\n'; exit 0; }\nif [ -e /etc/kubernetes/admin.conf ] || [ -e /etc/kubernetes/super-admin.conf ] || [ -e /etc/kubernetes/manifests/kube-apiserver.yaml ]; then printf 'control-plane\\n'; exit 0; fi\nuuid=$(tr '[:upper:]' '[:lower:]' < /sys/class/dmi/id/product_uuid 2>/dev/null | tr -d '\\r\\n')\nprintf '%%s\\n' \"$uuid\" | grep -Eq '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' || { printf 'identity-unavailable\\n'; exit 0; }\nbase='/var/lib/rlark/node-operations/%s.stale-reset'\nif [ -e \"$base.done\" ] && [ ! -e /etc/kubernetes/kubelet.conf ] && [ ! -e /etc/kubernetes/bootstrap-kubelet.conf ]; then printf 'completed %%s\\n' \"$uuid\"; elif [ -e \"$base\" ]; then printf 'attempted %%s\\n' \"$uuid\"; elif [ -e /etc/kubernetes/kubelet.conf ] || [ -e /etc/kubernetes/bootstrap-kubelet.conf ]; then printf 'configured %%s\\n' \"$uuid\"; else printf 'clean %%s\\n' \"$uuid\"; fi\n", nodeName, operationUID), nil
}

func normalizeSystemUUID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if !systemUUIDPattern.MatchString(value) || strings.Trim(value, "0-") == "" {
		return ""
	}
	return value
}

func parseNodeStaleResetObservation(output []byte) (string, string, error) {
	fields := strings.Fields(string(output))
	if len(fields) == 1 {
		switch fields[0] {
		case "hostname-mismatch":
			return "", "", ErrStaleResetHostMismatch
		case "control-plane":
			return "", "", ErrStaleResetControlPlane
		case "identity-unavailable":
			return "", "", ErrStaleResetIdentityUnavailable
		}
	}
	if len(fields) != 2 {
		return "", "", errors.New("invalid stale kubeadm reset observation")
	}
	state, systemUUID := fields[0], normalizeSystemUUID(fields[1])
	if systemUUID == "" {
		return "", "", ErrStaleResetIdentityUnavailable
	}
	switch state {
	case "configured", "clean", "attempted", "completed":
		return state, systemUUID, nil
	default:
		return "", "", errors.New("invalid stale kubeadm reset observation")
	}
}

func NodeStaleResetAttemptScript(operationUID, nodeName, expectedSystemUUID string) (string, error) {
	if err := validateNodeOperationUID(operationUID); err != nil {
		return "", err
	}
	if len(validation.IsDNS1123Subdomain(nodeName)) != 0 {
		return "", errors.New("invalid Kubernetes node name")
	}
	expectedSystemUUID = normalizeSystemUUID(expectedSystemUUID)
	if expectedSystemUUID == "" {
		return "", ErrStaleResetIdentityUnavailable
	}
	return fmt.Sprintf("set -eu\numask 077\nexpected='%s'\nactual=$(hostname -s | tr '[:upper:]' '[:lower:]')\n[ \"$actual\" = \"$expected\" ] || exit 44\nif [ -e /etc/kubernetes/admin.conf ] || [ -e /etc/kubernetes/super-admin.conf ] || [ -e /etc/kubernetes/manifests/kube-apiserver.yaml ]; then exit 45; fi\nuuid=$(tr '[:upper:]' '[:lower:]' < /sys/class/dmi/id/product_uuid 2>/dev/null | tr -d '\\r\\n')\n[ \"$uuid\" = '%s' ] || exit 47\n[ -e /etc/kubernetes/kubelet.conf ] || [ -e /etc/kubernetes/bootstrap-kubelet.conf ] || exit 46\nmkdir -p /var/lib/rlark/node-operations\nbase='/var/lib/rlark/node-operations/%s.stale-reset'\n[ ! -e \"$base\" ] || exit 43\n(set -C; : > \"$base\") || exit 43\nkubeadm reset --force\n[ ! -e /etc/kubernetes/kubelet.conf ] && [ ! -e /etc/kubernetes/bootstrap-kubelet.conf ]\n: > \"$base.done\"\nprintf 'completed\\n'\n", nodeName, expectedSystemUUID, operationUID), nil
}
