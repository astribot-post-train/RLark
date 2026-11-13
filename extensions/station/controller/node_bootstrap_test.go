package controller

import (
	"bytes"
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestNodeBootstrapCredentialReusesOwnedTokenAndRejectsCollision(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	client := fake.NewSimpleClientset()
	first, err := ensureNodeBootstrapToken(ctx, client, "operation-uid", now, bytes.NewReader(make([]byte, 8)))
	if err != nil {
		t.Fatal(err)
	}
	second, err := ensureNodeBootstrapToken(ctx, client, "operation-uid", now.Add(time.Minute), bytes.NewReader(nil))
	if err != nil || first != second {
		t.Fatalf("token not reused: %v", err)
	}
	secrets, err := client.CoreV1().Secrets(metav1.NamespaceSystem).List(ctx, metav1.ListOptions{})
	if err != nil || len(secrets.Items) != 1 {
		t.Fatalf("secret list: %v", err)
	}
	secret := secrets.Items[0].DeepCopy()
	secret.Annotations[bootstrapOperationAnnotation] = "other-operation"
	if _, err := client.CoreV1().Secrets(metav1.NamespaceSystem).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureNodeBootstrapToken(ctx, client, "operation-uid", now, bytes.NewReader(nil)); err == nil {
		t.Fatal("adopted another operation's credential")
	}
}

func TestRevokeNodeBootstrapCredentialRequiresOwnership(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	client := fake.NewSimpleClientset()
	if _, err := ensureNodeBootstrapToken(ctx, client, "uid", now, bytes.NewReader(make([]byte, 8))); err != nil {
		t.Fatal(err)
	}
	list, _ := client.CoreV1().Secrets(metav1.NamespaceSystem).List(ctx, metav1.ListOptions{})
	secret := list.Items[0].DeepCopy()
	secret.UID = types.UID("secret-uid")
	secret.ResourceVersion = "1"
	if _, err := client.CoreV1().Secrets(metav1.NamespaceSystem).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := RevokeNodeBootstrapToken(ctx, client, "uid"); err != nil {
		t.Fatal(err)
	}
	if err := RevokeNodeBootstrapToken(ctx, client, "uid"); err != nil {
		t.Fatalf("repeat revoke: %v", err)
	}
}

func TestNodeBootstrapCredentialRejectsExpiredAndMalformed(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	client := fake.NewSimpleClientset()
	if _, err := ensureNodeBootstrapToken(ctx, client, "uid", now, bytes.NewReader(make([]byte, 8))); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureNodeBootstrapToken(ctx, client, "uid", now.Add(15*time.Minute), bytes.NewReader(nil)); err == nil {
		t.Fatal("accepted expired credential")
	}
	secrets, _ := client.CoreV1().Secrets(metav1.NamespaceSystem).List(ctx, metav1.ListOptions{})
	secret := secrets.Items[0].DeepCopy()
	secret.Data["token-secret"] = []byte("invalid")
	if _, err := client.CoreV1().Secrets(metav1.NamespaceSystem).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureNodeBootstrapToken(ctx, client, "uid", now, bytes.NewReader(nil)); err == nil {
		t.Fatal("accepted malformed credential")
	}
}
