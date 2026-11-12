package controller

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// NodeJoinScript generates a fixed shell script. Every interpolated value is
// validated before use; the caller sends the script through SSH stdin.
func NodeJoinScript(nodeName string, discovery NodeJoinDiscovery, token string) (string, error) {
	if len(validation.IsDNS1123Subdomain(nodeName)) != 0 {
		return "", errors.New("invalid Kubernetes node name")
	}
	host, port, err := net.SplitHostPort(discovery.Endpoint)
	if err != nil || host == "" || (net.ParseIP(host) == nil && len(validation.IsDNS1123Subdomain(host)) != 0) {
		return "", errors.New("invalid control plane endpoint")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", errors.New("invalid control plane port")
	}
	if !strings.HasPrefix(discovery.CAHash, "sha256:") || len(discovery.CAHash) != 71 {
		return "", errors.New("invalid discovery CA hash")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(discovery.CAHash, "sha256:")); err != nil {
		return "", errors.New("invalid discovery CA hash")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 || len(parts[0]) != 6 || len(parts[1]) != 16 {
		return "", errors.New("invalid bootstrap token")
	}
	for _, part := range parts {
		if _, err := hex.DecodeString(part); err != nil {
			return "", errors.New("invalid bootstrap token")
		}
	}
	criSocket := discovery.CRISocket
	if criSocket == "" {
		criSocket = "/run/containerd/containerd.sock"
	}
	if criSocket != "/run/containerd/containerd.sock" && criSocket != "/run/containerd-fleet/containerd.sock" {
		return "", errors.New("unsupported containerd socket")
	}
	return fmt.Sprintf("set -eu\nif [ \"$(sysctl -n net.ipv4.ip_forward)\" != 1 ]; then printf 'net.ipv4.ip_forward must be 1 before Kubernetes Join\\n' >&2; exit 47; fi\nif [ -e /etc/kubernetes/kubelet.conf ] || [ -e /etc/kubernetes/bootstrap-kubelet.conf ]; then exit 42; fi\nexec kubeadm join '%s' --cri-socket 'unix://%s' --token '%s' --discovery-token-ca-cert-hash '%s' --node-name '%s'\n", discovery.Endpoint, criSocket, token, discovery.CAHash, nodeName), nil
}

// NodeJoinAttemptScript writes an operation marker before kubeadm runs. A
// repeated attempt never reruns kubeadm: callers must observe the resulting
// kubelet identity or report an unknown outcome for manual recovery.
func NodeJoinAttemptScript(operationUID, nodeName string, discovery NodeJoinDiscovery, token string) (string, error) {
	if err := validateNodeOperationUID(operationUID); err != nil {
		return "", err
	}
	join, err := NodeJoinScript(nodeName, discovery, token)
	if err != nil {
		return "", err
	}
	// The marker contains no token. kubeadm receives the short-lived token as an
	// argument, so privileged users on the target host can observe it briefly.
	return fmt.Sprintf("set -eu\numask 077\nmkdir -p /var/lib/rlark/node-operations\nmarker='/var/lib/rlark/node-operations/%s.join'\nif [ -e \"$marker\" ]; then exit 43; fi\nif [ \"$(sysctl -n net.ipv4.ip_forward)\" != 1 ]; then printf 'net.ipv4.ip_forward must be 1 before Kubernetes Join\\n' >&2; exit 47; fi\nif [ -e /etc/kubernetes/kubelet.conf ] || [ -e /etc/kubernetes/bootstrap-kubelet.conf ]; then exit 42; fi\n(set -C; : > \"$marker\") || exit 43\n%s", operationUID, join), nil
}

func validateNodeOperationUID(operationUID string) error {
	if len(operationUID) != 36 || strings.Count(operationUID, "-") != 4 {
		return errors.New("invalid operation UID")
	}
	for i, c := range operationUID {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return errors.New("invalid operation UID")
			}
		} else if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return errors.New("invalid operation UID")
		}
	}
	return nil
}
