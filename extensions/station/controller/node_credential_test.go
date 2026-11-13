package controller

import (
	"context"
	"testing"
	"time"

	stationv1alpha1 "github.com/rlinf/rlark/extensions/station/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNodeCredentialRequiresExactOwnerAndExpiry(t *testing.T) {
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	op := &stationv1alpha1.NodeOperation{ObjectMeta: metav1.ObjectMeta{Name: "op", Namespace: "ns", UID: "uid"}, Spec: stationv1alpha1.NodeOperationSpec{CredentialSecret: "auth"}}
	secret := testNodeCredentialSecret(op, now.Add(time.Minute))
	secret.Data = map[string][]byte{"ssh-password": []byte("test")}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	check := func(secret *corev1.Secret, wantErr bool) {
		t.Helper()
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
		password, _, err := (NodeSecretCredentialSource{Reader: c, Now: func() time.Time { return now }}).SSHCredential(context.Background(), op)
		if (err != nil) != wantErr {
			t.Fatalf("error: %v", err)
		}
		if !wantErr {
			password[0] = 'X'
			if string(secret.Data["ssh-password"]) != "test" {
				t.Fatal("returned Secret bytes alias source")
			}
		}
	}
	check(secret, false)
	wrongOwner := secret.DeepCopy()
	wrongOwner.Annotations[nodeOperationCredentialUIDAnnotation] = "other"
	check(wrongOwner, true)
	expired := secret.DeepCopy()
	expired.Annotations[nodeCredentialExpiryAnnotation] = now.Format(time.RFC3339)
	check(expired, true)
	both := secret.DeepCopy()
	both.Data["ssh-private-key"] = []byte("key")
	check(both, true)
}

func TestNodeSudoCredentialIsIndependent(t *testing.T) {
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	op := &stationv1alpha1.NodeOperation{ObjectMeta: metav1.ObjectMeta{Name: "op", Namespace: "ns", UID: "uid"}, Spec: stationv1alpha1.NodeOperationSpec{CredentialSecret: "auth"}}
	secret := testNodeCredentialSecret(op, now.Add(time.Minute))
	secret.Data = map[string][]byte{"ssh-private-key": []byte("key"), "sudo-password": []byte("sudo")}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	source := NodeSecretCredentialSource{Reader: c, Now: func() time.Time { return now }}
	password, err := source.SudoCredential(context.Background(), op)
	if err != nil || string(password) != "sudo" {
		t.Fatalf("sudo credential: %v", err)
	}
	password[0] = 'X'
	if string(secret.Data["sudo-password"]) != "sudo" {
		t.Fatal("returned password aliases Secret")
	}
}

func TestCleanupNodeCredentialChecksOwnerAndWaitsForDeletion(t *testing.T) {
	op := &stationv1alpha1.NodeOperation{ObjectMeta: metav1.ObjectMeta{Name: "op", Namespace: "ns", UID: "uid"}, Spec: stationv1alpha1.NodeOperationSpec{CredentialSecret: "auth"}}
	secret := testNodeCredentialSecret(op, time.Now().Add(time.Minute))
	secret.UID = "secret-uid"
	secret.ResourceVersion = "1"
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	source := NodeSecretCredentialSource{Reader: c, Writer: c}
	if done, err := source.Cleanup(context.Background(), op); err != nil || done {
		t.Fatalf("delete: done=%v err=%v", done, err)
	}
	if done, err := source.Cleanup(context.Background(), op); err != nil || !done {
		t.Fatalf("absence: done=%v err=%v", done, err)
	}
	foreign := secret.DeepCopy()
	foreign.Annotations[nodeOperationCredentialUIDAnnotation] = "other"
	foreign.ResourceVersion = ""
	if err := c.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Cleanup(context.Background(), op); err == nil {
		t.Fatal("deleted foreign credential")
	}
}

func testNodeCredentialSecret(op *stationv1alpha1.NodeOperation, expires time.Time) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: op.Spec.CredentialSecret, Namespace: nodeOperationCredentialNamespace,
		Annotations: map[string]string{
			nodeCredentialExpiryAnnotation:             expires.Format(time.RFC3339),
			nodeOperationCredentialNameAnnotation:      op.Name,
			nodeOperationCredentialNamespaceAnnotation: op.Namespace,
			nodeOperationCredentialUIDAnnotation:       string(op.UID),
		},
	}}
}
