package storageconfig

import (
	"context"
	"fmt"

	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

// SyncEndpointSecrets makes the storage's endpoint declarations the
// rendered ones: it creates and updates them and deletes those of clusters
// that no longer have an endpoint of their own (or of a deleted storage, when
// stored is nil). A changed SMB password render alone is not a change.
func SyncEndpointSecrets(ctx context.Context, client kubernetes.Interface, namespace, id string, stored *StoredClass, identity *v1.Secret) error {
	existing, err := client.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set{EndpointLabel: id}.AsSelector().String(),
	})
	if err != nil {
		return fmt.Errorf("list storage endpoint declarations: %w", err)
	}
	current := make(map[string]*v1.Secret, len(existing.Items))
	for i := range existing.Items {
		current[existing.Items[i].Name] = &existing.Items[i]
	}
	var desired []*v1.Secret
	if stored != nil {
		if desired, err = RenderEndpoints(namespace, id, *stored); err != nil {
			return err
		}
	}
	for _, secret := range desired {
		old, found := current[secret.Name]
		delete(current, secret.Name)
		if !found {
			tenant.SetIdentity(secret, tenant.IDOf(identity), identity.Annotations[tenant.CreatedByAnnotation])
			if _, err := client.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
				return fmt.Errorf("create storage endpoint declaration %s: %w", secret.Name, err)
			}
			continue
		}
		if RenderKey(old.Data) == RenderKey(secret.Data) {
			continue
		}
		old.Data = secret.Data
		if _, err := client.CoreV1().Secrets(namespace).Update(ctx, old, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("update storage endpoint declaration %s: %w", old.Name, err)
		}
	}
	for name, secret := range current {
		if err := deleteSecret(ctx, client, secret); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete storage endpoint declaration %s: %w", name, err)
		}
	}
	return nil
}
