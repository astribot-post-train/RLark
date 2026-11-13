package stationidentity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

// The cluster CA of the test: any certificate does, the hash is of its public key.
const testClusterInfo = `apiVersion: v1
kind: Config
clusters:
- name: ""
  cluster:
    server: https://10.11.14.128:16443
    certificate-authority-data: LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCk1JSUJlVENDQVIrZ0F3SUJBZ0lVQ1FQRkhWbHlkZ1ZMTHhWSUcwc1B5OVQwN2Nnd0NnWUlLb1pJemowRUF3SXcKRWpFUU1BNEdBMVVFQXd3SGRHVnpkQzFqWVRBZUZ3MHlOakV4TVRJd09UTTFNelphRncwek5qRXhNRGt3T1RNMQpNelphTUJJeEVEQU9CZ05WQkFNTUIzUmxjM1F0WTJFd1dUQVRCZ2NxaGtqT1BRSUJCZ2dxaGtqT1BRTUJCd05DCkFBUjdleVkyd0dYSkoyYkRBdzI2WGpwSlFENm1GVGh0YkFTYUpBTFBWVCtaR3JNMHBpaVg1Y05UeWN5SDlyNkMKajY0VnArUVlhaGVrZm1nazhtRmlaeDE3bzFNd1VUQWRCZ05WSFE0RUZnUVU5NklpaU5MNjBUN1RzQ0xIQ0FRRwozRXAveWlVd0h3WURWUjBqQkJnd0ZvQVU5NklpaU5MNjBUN1RzQ0xIQ0FRRzNFcC95aVV3RHdZRFZSMFRBUUgvCkJBVXdBd0VCL3pBS0JnZ3Foa2pPUFFRREFnTklBREJGQWlBeTlwQzlKMjFycHVKZDdUaC9zUXhUZjlxMkV4NGYKVlJSd25Gc3FqbFAxeHdJaEFJQ1k4eUZLK0x6eVhQSnp1cmJBelk4SDNVVVMxaWJuVWxULzZZbGw4MGVvCi0tLS0tRU5EIENFUlRJRklDQVRFLS0tLS0K
`

func TestBootstrapRedeemsACodeOnceForOneKey(t *testing.T) {
	code, key, other := "k7m2p9xq", strings.Repeat("A", 43)+"=", strings.Repeat("B", 43)+"="
	digest := sha256.Sum256([]byte(code))
	now := time.Unix(1_800_000_000, 0)
	admission := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeAdmission",
		"metadata": map[string]interface{}{"name": "s1u-01-x86", "namespace": "rlark-local-dev-gpu"},
		"spec": map[string]interface{}{
			"clusterID": "rlark-local-dev-gpu", "codeHash": hex.EncodeToString(digest[:]), "apiServer": "10.11.14.128:16443",
			"expiresAt": now.Add(time.Hour).UTC().Format(time.RFC3339),
			"labels":    map[string]interface{}{"rlark.io/robot-id": "s1u-01", "rlark.io/node-category": "robot"},
			"taints":    []interface{}{"rlark.io/node-category=robot:NoSchedule"},
		},
	}}
	management := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{nodeAdmissionGVR: "NodeAdmissionList"}, admission)
	member := kubefake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-public", Name: "cluster-info"}, Data: map[string]string{"kubeconfig": testClusterInfo}})
	b := &Bootstrap{Management: management, Now: func() time.Time { return now },
		Member: func(context.Context, string) (kubernetes.Interface, error) { return member, nil }}
	ctx := context.Background()

	if answer, status, err := b.Redeem(ctx, "wrongcode", key); err != nil || status != http.StatusNotFound || answer.State != "rejected" {
		t.Fatalf("unknown code: %+v %d %v", answer, status, err)
	}
	if answer, status, err := b.Redeem(ctx, code, key); err != nil || status != http.StatusAccepted || answer.State != "pending" {
		t.Fatalf("first redemption: %+v %d %v", answer, status, err)
	}
	if answer, status, err := b.Redeem(ctx, code, other); err != nil || status != http.StatusConflict {
		t.Fatalf("a second machine must not redeem the code: %+v %d %v", answer, status, err)
	}
	if answer, status, err := b.Redeem(ctx, code, key); err != nil || status != http.StatusAccepted || answer.Join != nil {
		t.Fatalf("no join token before the tunnel is computed: %+v %d %v", answer, status, err)
	}

	resource := management.Resource(nodeAdmissionGVR).Namespace("rlark-local-dev-gpu")
	stored, err := resource.Get(ctx, "s1u-01-x86", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stored.Object["status"] = map[string]interface{}{"phase": "Redeemed", "address": "10.254.240.3", "publicKey": key, "config": `{"address":"10.254.240.3/32"}`}
	if _, err := resource.Update(ctx, stored, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	answer, status, err := b.Redeem(ctx, code, key)
	if err != nil || status != http.StatusOK || answer.State != "ready" || answer.NodeName != "s1u-01-x86" || answer.TunnelAddress != "10.254.240.3" ||
		answer.Join.APIServer != "10.11.14.128:16443" || len(answer.Join.Token) != 23 || !strings.HasPrefix(answer.Join.CACertHash, "sha256:") ||
		answer.Labels != "rlark.io/node-category=robot,rlark.io/robot-id=s1u-01" || answer.Taints != "rlark.io/node-category=robot:NoSchedule" {
		t.Fatalf("ready answer: %+v %d %v", answer, status, err)
	}
	secrets, err := member.CoreV1().Secrets("kube-system").List(ctx, metav1.ListOptions{})
	if err != nil || len(secrets.Items) != 1 || secrets.Items[0].Type != corev1.SecretTypeBootstrapToken ||
		secrets.Items[0].StringData["expiration"] != now.Add(30*time.Minute).UTC().Format(time.RFC3339) {
		t.Fatalf("join token secret: %+v %v", secrets.Items, err)
	}

	// A node that joins again got a new code for its admission: "Joined" from
	// before is no answer until station-controller has seen this redemption.
	stored, _ = resource.Get(ctx, "s1u-01-x86", metav1.GetOptions{})
	stored.SetGeneration(5)
	stored.Object["status"] = map[string]interface{}{"phase": "Joined", "address": "10.254.240.3", "observedGeneration": int64(4)}
	if _, err := resource.Update(ctx, stored, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if answer, status, err := b.Redeem(ctx, code, key); err != nil || status != http.StatusAccepted || answer.State != "pending" {
		t.Fatalf("a status older than the redemption: %+v %d %v", answer, status, err)
	}
	_ = unstructured.SetNestedField(stored.Object, int64(5), "status", "observedGeneration")
	if _, err := resource.Update(ctx, stored, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if answer, status, err := b.Redeem(ctx, code, key); err != nil || status != http.StatusGone {
		t.Fatalf("joined: %+v %d %v", answer, status, err)
	}

	// After the time is up an unredeemed code is dead.
	stored, _ = resource.Get(ctx, "s1u-01-x86", metav1.GetOptions{})
	unstructured.RemoveNestedField(stored.Object, "spec", "publicKey")
	if _, err := resource.Update(ctx, stored, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	if answer, status, err := b.Redeem(ctx, code, key); err != nil || status != http.StatusGone {
		t.Fatalf("expired code: %+v %d %v", answer, status, err)
	}
}
