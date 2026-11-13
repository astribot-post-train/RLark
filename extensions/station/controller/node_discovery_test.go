package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestReadNodeJoinDiscovery(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	kubeconfig := []byte(fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: member\n  cluster:\n    server: https://10.0.0.1:6443\n    certificate-authority-data: %s\ncontexts:\n- name: member\n  context:\n    cluster: member\n    user: member\ncurrent-context: member\nusers:\n- name: member\n  user:\n    token: test\n", base64.StdEncoding.EncodeToString(ca)))
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kubeadm-config", Namespace: metav1.NamespaceSystem}, Data: map[string]string{"ClusterConfiguration": "controlPlaneEndpoint: '10.0.0.1:6443'\nkubernetesVersion: v1.31.14\n"}}
	client := fake.NewSimpleClientset(cm)
	result, err := ReadNodeJoinDiscovery(context.Background(), client, kubeconfig)
	if err != nil || result.Endpoint != "10.0.0.1:6443" || !strings.HasPrefix(result.CAHash, "sha256:") || len(result.CAHash) != 71 {
		t.Fatalf("discovery: %+v %v", result, err)
	}
	cm.Data["ClusterConfiguration"] = "controlPlaneEndpoint: 'bad endpoint'"
	if _, err := client.CoreV1().ConfigMaps(metav1.NamespaceSystem).Update(context.Background(), cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadNodeJoinDiscovery(context.Background(), client, kubeconfig); err == nil {
		t.Fatal("accepted invalid endpoint")
	}
}
