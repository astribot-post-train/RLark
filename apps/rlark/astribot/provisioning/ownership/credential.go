// Package ownership records the identity of credentials written by cluster provisioning.
package ownership

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/util/retry"
)

const (
	ProvisionUID  = "rlark.io/cluster-provision-uid"
	credentialUID = "rlark.io/provisioned-credential-uid"
)

// WriteCredential claims a new Secret on create. Retries can finish the UID
// receipt on an already claimed Secret, but cannot adopt one from another writer.
func WriteCredential(ctx context.Context, secrets typedcore.SecretInterface, desired *corev1.Secret, owner types.UID) error {
	if owner == "" {
		return fmt.Errorf("cluster provision UID is required")
	}
	return retry.OnError(retry.DefaultRetry, func(err error) bool {
		return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err)
	}, func() error {
		current, err := secrets.Get(ctx, desired.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			candidate := desired.DeepCopy()
			metav1.SetMetaDataAnnotation(&candidate.ObjectMeta, ProvisionUID, string(owner))
			current, err = secrets.Create(ctx, candidate, metav1.CreateOptions{})
		}
		if err != nil {
			return err
		}
		receipt := current.Annotations[credentialUID]
		if current.UID == "" || current.DeletionTimestamp != nil ||
			current.Annotations[ProvisionUID] != string(owner) || receipt != "" && receipt != string(current.UID) {
			return apierrors.NewConflict(corev1.Resource("secrets"), desired.Name, fmt.Errorf("credential does not belong to this cluster provision"))
		}
		current.Data = desired.Data
		metav1.SetMetaDataAnnotation(&current.ObjectMeta, credentialUID, string(current.UID))
		_, err = secrets.Update(ctx, current, metav1.UpdateOptions{})
		return err
	})
}

func CredentialMatches(secret *corev1.Secret, owner types.UID) bool {
	return owner != "" && secret.UID != "" && secret.Annotations[ProvisionUID] == string(owner) &&
		secret.Annotations[credentialUID] == string(secret.UID)
}

// UnbindCredential hands a provisioned credential back to an independent writer.
func UnbindCredential(secret *corev1.Secret) {
	delete(secret.Annotations, ProvisionUID)
	delete(secret.Annotations, credentialUID)
}
