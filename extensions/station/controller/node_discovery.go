package controller

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

type NodeJoinDiscovery struct {
	Endpoint  string
	CAHash    string
	CRISocket string
}

// ReadNodeJoinDiscovery obtains the endpoint from kubeadm configuration and
// pins the CA embedded in the already validated member kubeconfig.
func ReadNodeJoinDiscovery(ctx context.Context, member kubernetes.Interface, kubeconfig []byte) (NodeJoinDiscovery, error) {
	if member == nil {
		return NodeJoinDiscovery{}, errors.New("member Kubernetes client is required")
	}
	config, err := secureRESTConfigFromKubeconfig(kubeconfig)
	if err != nil {
		return NodeJoinDiscovery{}, fmt.Errorf("validate member kubeconfig: %w", err)
	}
	if len(config.CAData) == 0 {
		return NodeJoinDiscovery{}, errors.New("member kubeconfig must contain an embedded CA")
	}
	block, remaining := pem.Decode(config.CAData)
	if block == nil || block.Type != "CERTIFICATE" || len(strings.TrimSpace(string(remaining))) != 0 {
		return NodeJoinDiscovery{}, errors.New("member kubeconfig must contain exactly one CA certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !cert.IsCA {
		return NodeJoinDiscovery{}, errors.New("member kubeconfig CA certificate is invalid")
	}
	digest := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	cm, err := member.CoreV1().ConfigMaps(metav1.NamespaceSystem).Get(ctx, "kubeadm-config", metav1.GetOptions{})
	if err != nil {
		return NodeJoinDiscovery{}, fmt.Errorf("read kubeadm cluster configuration: %w", err)
	}
	var cluster struct {
		ControlPlaneEndpoint string `json:"controlPlaneEndpoint"`
	}
	if err := yaml.Unmarshal([]byte(cm.Data["ClusterConfiguration"]), &cluster); err != nil {
		return NodeJoinDiscovery{}, errors.New("invalid kubeadm cluster configuration")
	}
	host, port, err := net.SplitHostPort(cluster.ControlPlaneEndpoint)
	if err != nil || host == "" || strings.ContainsAny(host, " \t\r\n") {
		return NodeJoinDiscovery{}, errors.New("invalid kubeadm controlPlaneEndpoint")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return NodeJoinDiscovery{}, errors.New("invalid kubeadm controlPlaneEndpoint port")
	}
	return NodeJoinDiscovery{Endpoint: cluster.ControlPlaneEndpoint, CAHash: "sha256:" + hex.EncodeToString(digest[:])}, nil
}
