package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const bootstrapOperationAnnotation = "station.rlark.io/node-operation-uid"

// EnsureNodeBootstrapToken returns a short-lived kubeadm token owned by one
// operation. The token is stored only in the member cluster's bootstrap Secret.
func EnsureNodeBootstrapToken(ctx context.Context, member kubernetes.Interface, operationUID types.UID, now time.Time) (string, error) {
	return ensureNodeBootstrapToken(ctx, member, operationUID, now, rand.Reader)
}

func ensureNodeBootstrapToken(ctx context.Context, member kubernetes.Interface, operationUID types.UID, now time.Time, randomness io.Reader) (string, error) {
	if member == nil || operationUID == "" || randomness == nil {
		return "", errors.New("member client and operation UID are required")
	}
	digest := sha256.Sum256([]byte(operationUID))
	id := hex.EncodeToString(digest[:3])
	secrets := member.CoreV1().Secrets(metav1.NamespaceSystem)
	name := "bootstrap-token-" + id
	secret, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		var random [8]byte
		if _, err := io.ReadFull(randomness, random[:]); err != nil {
			return "", errors.New("generate bootstrap credential")
		}
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: metav1.NamespaceSystem, Annotations: map[string]string{bootstrapOperationAnnotation: string(operationUID)}}, Type: corev1.SecretType("bootstrap.kubernetes.io/token"), Data: map[string][]byte{
			"token-id": []byte(id), "token-secret": []byte(hex.EncodeToString(random[:])), "expiration": []byte(now.Add(15 * time.Minute).UTC().Format(time.RFC3339)),
			"usage-bootstrap-authentication": []byte("true"), "usage-bootstrap-signing": []byte("true"), "auth-extra-groups": []byte("system:bootstrappers:kubeadm:default-node-token"),
		}}
		secret, err = secrets.Create(ctx, secret, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			secret, err = secrets.Get(ctx, name, metav1.GetOptions{})
		}
	}
	if err != nil {
		return "", fmt.Errorf("read or create bootstrap credential: %w", err)
	}
	if secret.Type != corev1.SecretType("bootstrap.kubernetes.io/token") || secret.Annotations[bootstrapOperationAnnotation] != string(operationUID) || string(secret.Data["token-id"]) != id {
		return "", errors.New("bootstrap credential ownership conflict")
	}
	if string(secret.Data["usage-bootstrap-authentication"]) != "true" || string(secret.Data["usage-bootstrap-signing"]) != "true" || string(secret.Data["auth-extra-groups"]) != "system:bootstrappers:kubeadm:default-node-token" {
		return "", errors.New("bootstrap credential usage is invalid")
	}
	expires, err := time.Parse(time.RFC3339, string(secret.Data["expiration"]))
	if err != nil || !expires.After(now) || expires.After(now.Add(15*time.Minute)) {
		return "", errors.New("bootstrap credential expired or has invalid expiry")
	}
	value := string(secret.Data["token-secret"])
	if len(value) != 16 {
		return "", errors.New("bootstrap credential is invalid")
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", errors.New("bootstrap credential is invalid")
	}
	return id + "." + value, nil
}

// RevokeNodeBootstrapToken removes only the credential owned by this operation.
// Absence is success; UID and resourceVersion preconditions protect replacements.
func RevokeNodeBootstrapToken(ctx context.Context, member kubernetes.Interface, operationUID types.UID) error {
	if member == nil || operationUID == "" {
		return errors.New("member client and operation UID are required")
	}
	digest := sha256.Sum256([]byte(operationUID))
	name := "bootstrap-token-" + hex.EncodeToString(digest[:3])
	secrets := member.CoreV1().Secrets(metav1.NamespaceSystem)
	secret, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read bootstrap credential: %w", err)
	}
	if secret.Annotations[bootstrapOperationAnnotation] != string(operationUID) || secret.UID == "" || secret.ResourceVersion == "" {
		return errors.New("bootstrap credential ownership conflict")
	}
	err = secrets.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &secret.UID, ResourceVersion: &secret.ResourceVersion}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("revoke bootstrap credential: %w", err)
	}
	return nil
}
