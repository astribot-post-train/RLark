package stationidentity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"reflect"
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
	// The machine is installed anew: the management plane knows a node of its name.
	known := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rlinf.io/v1alpha1", "kind": "Node",
		"metadata": map[string]interface{}{"name": "s1u-01-x86", "namespace": "rlark-local-dev-gpu",
			"labels": map[string]interface{}{"rlark.io/robot-id": "before", "rlark.io/robot-model": "s1u"}},
	}}
	management := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{nodeAdmissionGVR: "NodeAdmissionList", nodeEnrollmentGVR: "NodeEnrollmentList"}, admission, known)
	member := kubefake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-public", Name: "cluster-info"}, Data: map[string]string{"kubeconfig": testClusterInfo}})
	b := &Bootstrap{Management: management, Now: func() time.Time { return now },
		Member: func(context.Context, string) (kubernetes.Interface, error) { return member, nil }}
	ctx := context.Background()

	if answer, status, err := b.Redeem(ctx, "wrongcode", key, ""); err != nil || status != http.StatusNotFound || answer.State != "rejected" {
		t.Fatalf("unknown code: %+v %d %v", answer, status, err)
	}
	if answer, status, err := b.Redeem(ctx, code, key, ""); err != nil || status != http.StatusAccepted || answer.State != "pending" {
		t.Fatalf("first redemption: %+v %d %v", answer, status, err)
	}
	if answer, status, err := b.Redeem(ctx, code, other, ""); err != nil || status != http.StatusConflict {
		t.Fatalf("a second machine must not redeem the code: %+v %d %v", answer, status, err)
	}
	if answer, status, err := b.Redeem(ctx, code, key, ""); err != nil || status != http.StatusAccepted || answer.Join != nil {
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
	answer, status, err := b.Redeem(ctx, code, key, "")
	if err != nil || status != http.StatusOK || answer.State != "ready" || answer.NodeName != "s1u-01-x86" || answer.TunnelAddress != "10.254.240.3" ||
		answer.Join.APIServer != "10.11.14.128:16443" || len(answer.Join.Token) != 23 || !strings.HasPrefix(answer.Join.CACertHash, "sha256:") ||
		answer.Labels != "rlark.io/node-category=robot,rlark.io/robot-id=s1u-01" || answer.Taints != "rlark.io/node-category=robot:NoSchedule" {
		t.Fatalf("ready answer: %+v %d %v", answer, status, err)
	}
	node, err := management.Resource(rlarkNodeGVR).Namespace("rlark-local-dev-gpu").Get(ctx, "s1u-01-x86", metav1.GetOptions{})
	if want := map[string]string{"rlark.io/robot-id": "s1u-01", "rlark.io/node-category": "robot", "rlark.io/robot-model": "s1u"}; err != nil || !reflect.DeepEqual(node.GetLabels(), want) {
		t.Fatalf("labels of the management Node after the admission: %v %v", node.GetLabels(), err)
	}
	secrets, err := member.CoreV1().Secrets("kube-system").List(ctx, metav1.ListOptions{})
	if err != nil || len(secrets.Items) != 1 || secrets.Items[0].Type != corev1.SecretTypeBootstrapToken ||
		secrets.Items[0].StringData["expiration"] != now.Add(30*time.Minute).UTC().Format(time.RFC3339) {
		t.Fatalf("join token secret: %+v %v", secrets.Items, err)
	}

	// After the time is up an unredeemed code is dead.
	stored, _ = resource.Get(ctx, "s1u-01-x86", metav1.GetOptions{})
	unstructured.RemoveNestedField(stored.Object, "spec", "publicKey")
	if _, err := resource.Update(ctx, stored, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	if answer, status, err := b.Redeem(ctx, code, key, ""); err != nil || status != http.StatusGone {
		t.Fatalf("expired code: %+v %d %v", answer, status, err)
	}
}

func TestBootstrapAdmitsMachinesOfAClassWithOneCode(t *testing.T) {
	code, first, second, third := "s1ubatch", strings.Repeat("A", 43)+"=", strings.Repeat("B", 43)+"=", strings.Repeat("C", 43)+"="
	digest := sha256.Sum256([]byte(code))
	now := time.Unix(1_800_000_000, 0)
	class := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeClass", "metadata": map[string]interface{}{"name": "robot-s1u"},
		"spec": map[string]interface{}{
			"labels": map[string]interface{}{"rlark.io/robot-model": "s1u", "rlark.io/node-category": "robot"},
			"taints": []interface{}{"rlark.io/node-category=robot:NoSchedule"},
		},
	}}
	enrollment := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeEnrollment",
		"metadata": map[string]interface{}{"name": "s1u-batch-1", "namespace": "rlark-robot-s1u-01"},
		"spec": map[string]interface{}{
			"clusterID": "rlark-robot-s1u-01", "codeHash": hex.EncodeToString(digest[:]), "apiServer": "cp.rlark-robot-s1u-01.rlark.internal:6443",
			"expiresAt": now.Add(time.Hour).UTC().Format(time.RFC3339), "maxUses": int64(2), "nodeClass": "robot-s1u",
		},
	}}
	management := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{nodeAdmissionGVR: "NodeAdmissionList", nodeEnrollmentGVR: "NodeEnrollmentList"}, class, enrollment)
	member := kubefake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-public", Name: "cluster-info"}, Data: map[string]string{"kubeconfig": testClusterInfo}})
	b := &Bootstrap{Management: management, Now: func() time.Time { return now },
		Member: func(context.Context, string) (kubernetes.Interface, error) { return member, nil }}
	ctx := context.Background()
	admissions := management.Resource(nodeAdmissionGVR).Namespace("rlark-robot-s1u-01")

	if answer, status, err := b.Redeem(ctx, code, first, "S1U_03"); err != nil || status != http.StatusBadRequest {
		t.Fatalf("a name that cannot be a node name: %+v %d %v", answer, status, err)
	}
	for range 2 { // the machine calls until its tunnel is computed; it uses the code once
		if answer, status, err := b.Redeem(ctx, code, first, "s1u-03-x86"); err != nil || status != http.StatusAccepted || answer.State != "pending" {
			t.Fatalf("first machine: %+v %d %v", answer, status, err)
		}
	}
	if answer, status, err := b.Redeem(ctx, code, second, "s1u-03-x86"); err != nil || status != http.StatusConflict {
		t.Fatalf("a second machine under the first one's name: %+v %d %v", answer, status, err)
	}
	if answer, status, err := b.Redeem(ctx, code, second, "s1u-04-x86"); err != nil || status != http.StatusAccepted {
		t.Fatalf("second machine: %+v %d %v", answer, status, err)
	}
	if answer, status, err := b.Redeem(ctx, code, third, "s1u-05-x86"); err != nil || status != http.StatusGone {
		t.Fatalf("a third machine on a code for two: %+v %d %v", answer, status, err)
	}
	stored, err := management.Resource(nodeEnrollmentGVR).Namespace("rlark-robot-s1u-01").Get(ctx, "s1u-batch-1", metav1.GetOptions{})
	if uses, _, _ := unstructured.NestedInt64(stored.Object, "status", "uses"); err != nil || uses != 2 {
		t.Fatalf("uses after two machines: %d %v", uses, err)
	}

	admission, err := admissions.Get(ctx, "s1u-03-x86", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	spec := admission.Object["spec"].(map[string]interface{})
	if spec["publicKey"] != first || spec["clusterID"] != "rlark-robot-s1u-01" || spec["codeHash"] == hex.EncodeToString(digest[:]) ||
		admission.GetLabels()["station.rlark.io/enrollment"] != "s1u-batch-1" || admission.GetLabels()["station.rlark.io/node-class"] != "robot-s1u" {
		t.Fatalf("admission written for the first machine: %+v", admission.Object)
	}
	admission.Object["status"] = map[string]interface{}{"phase": "Redeemed", "address": "10.254.240.9", "publicKey": first, "config": `{"address":"10.254.240.9/32"}`}
	if _, err := admissions.Update(ctx, admission, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	// The code stays good for a machine it admitted after it has run out and expired.
	now = now.Add(2 * time.Hour)
	answer, status, err := b.Redeem(ctx, code, first, "s1u-03-x86")
	if err != nil || status != http.StatusOK || answer.State != "ready" || answer.NodeName != "s1u-03-x86" || answer.ClusterID != "rlark-robot-s1u-01" ||
		answer.Join.APIServer != "cp.rlark-robot-s1u-01.rlark.internal:6443" ||
		answer.Labels != "rlark.io/node-category=robot,rlark.io/robot-model=s1u" || answer.Taints != "rlark.io/node-category=robot:NoSchedule" {
		t.Fatalf("ready answer: %+v %d %v", answer, status, err)
	}

	// A node admitted one by one keeps its name against the code.
	if _, err := admissions.Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeAdmission", "metadata": map[string]interface{}{"name": "s1u-01-x86", "namespace": "rlark-robot-s1u-01"},
		"spec": map[string]interface{}{"clusterID": "rlark-robot-s1u-01", "codeHash": strings.Repeat("0", 64)},
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(-2 * time.Hour)
	stored.Object["spec"].(map[string]interface{})["maxUses"] = int64(3)
	if _, err := management.Resource(nodeEnrollmentGVR).Namespace("rlark-robot-s1u-01").Update(ctx, stored, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if answer, status, err := b.Redeem(ctx, code, third, "s1u-01-x86"); err != nil || status != http.StatusConflict {
		t.Fatalf("a machine under the name of an admitted node: %+v %d %v", answer, status, err)
	}
	stored.Object["spec"].(map[string]interface{})["revoked"] = true
	if _, err := management.Resource(nodeEnrollmentGVR).Namespace("rlark-robot-s1u-01").Update(ctx, stored, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if answer, status, err := b.Redeem(ctx, code, third, "s1u-05-x86"); err != nil || status != http.StatusGone {
		t.Fatalf("a new machine on a revoked code: %+v %d %v", answer, status, err)
	}
}
