package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	stationv1alpha1 "github.com/rlinf/rlark/extensions/station/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const nodeCredentialExpiryAnnotation = "station.rlark.io/credential-expires-at"
const nodeOperationCredentialNamespace = "station-operation-credentials"
const nodeOperationCredentialNameAnnotation = "station.rlark.io/operation-name"
const nodeOperationCredentialNamespaceAnnotation = "station.rlark.io/operation-namespace"
const nodeOperationCredentialUIDAnnotation = "station.rlark.io/operation-uid"

// NodeSecretCredentialSource reads only an unexpired Secret bound by annotations
// to the exact NodeOperation instance in a dedicated namespace.
type NodeSecretCredentialSource struct {
	Reader    client.Reader
	Writer    client.Client
	Namespace string
	Now       func() time.Time
}

// Cleanup deletes only the exact Secret instance owned by the operation. It
// returns done after the Secret has disappeared, not merely after Delete.
func (s NodeSecretCredentialSource) Cleanup(ctx context.Context, op *stationv1alpha1.NodeOperation) (bool, error) {
	if s.Reader == nil || s.Writer == nil || op == nil || op.UID == "" || op.Namespace == "" || op.Spec.CredentialSecret == "" {
		return false, errors.New("operation credential cleanup is not configured")
	}
	namespace := s.credentialNamespace()
	secret := &corev1.Secret{}
	if err := s.Reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: op.Spec.CredentialSecret}, secret); apierrors.IsNotFound(err) {
		return true, nil
	} else if err != nil {
		return false, fmt.Errorf("read operation credential for cleanup: %w", err)
	}
	if !nodeCredentialOwnedBy(secret, op) || secret.UID == "" || secret.ResourceVersion == "" {
		return false, errors.New("credential Secret ownership mismatch")
	}
	if secret.DeletionTimestamp != nil {
		return false, nil
	}
	uid, version := secret.UID, secret.ResourceVersion
	if err := s.Writer.Delete(ctx, secret, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version}}); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("delete operation credential: %w", err)
	}
	return false, nil
}

func nodeCredentialOwnedBy(secret *corev1.Secret, op *stationv1alpha1.NodeOperation) bool {
	return secret != nil && op != nil && secret.Namespace == nodeOperationCredentialNamespace &&
		secret.Annotations[nodeOperationCredentialNameAnnotation] == op.Name &&
		secret.Annotations[nodeOperationCredentialNamespaceAnnotation] == op.Namespace &&
		secret.Annotations[nodeOperationCredentialUIDAnnotation] == string(op.UID)
}

func (s NodeSecretCredentialSource) credentialNamespace() string {
	if s.Namespace != "" {
		return s.Namespace
	}
	return nodeOperationCredentialNamespace
}

func (s NodeSecretCredentialSource) SSHCredential(ctx context.Context, op *stationv1alpha1.NodeOperation) ([]byte, []byte, error) {
	secret, err := s.credentialSecret(ctx, op)
	if err != nil {
		return nil, nil, err
	}
	password, key := secret.Data["ssh-password"], secret.Data["ssh-private-key"]
	if (len(password) == 0) == (len(key) == 0) {
		return nil, nil, errors.New("credential Secret must contain exactly one SSH authentication method")
	}
	return append([]byte(nil), password...), append([]byte(nil), key...), nil
}

// SudoCredential returns nil for passwordless sudo or a distinct sudo password.
func (s NodeSecretCredentialSource) SudoCredential(ctx context.Context, op *stationv1alpha1.NodeOperation) ([]byte, error) {
	secret, err := s.credentialSecret(ctx, op)
	if err != nil {
		return nil, err
	}
	password, found := secret.Data["sudo-password"]
	if !found {
		return nil, nil
	}
	if len(password) == 0 {
		return nil, errors.New("sudo password is empty")
	}
	return append([]byte(nil), password...), nil
}

func (s NodeSecretCredentialSource) credentialSecret(ctx context.Context, op *stationv1alpha1.NodeOperation) (*corev1.Secret, error) {
	if s.Reader == nil || op == nil || op.UID == "" || op.Namespace == "" || op.Spec.CredentialSecret == "" {
		return nil, errors.New("operation credential reference is incomplete")
	}
	secret := &corev1.Secret{}
	if err := s.Reader.Get(ctx, types.NamespacedName{Namespace: s.credentialNamespace(), Name: op.Spec.CredentialSecret}, secret); err != nil {
		return nil, fmt.Errorf("read operation credential Secret: %w", err)
	}
	if !nodeCredentialOwnedBy(secret, op) {
		return nil, errors.New("credential Secret ownership mismatch")
	}
	expires, err := time.Parse(time.RFC3339, secret.Annotations[nodeCredentialExpiryAnnotation])
	if err != nil {
		return nil, errors.New("credential Secret expiry is invalid")
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	if !expires.After(now) {
		return nil, errors.New("credential Secret expired")
	}
	return secret, nil
}
