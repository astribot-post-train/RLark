package gateway

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/rlinf/rlark/apps/rlark/astribot/storageconfig"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
)

// syncStorageEndpointSecrets makes the storage's endpoint declarations the
// rendered ones: it creates and updates them and deletes those of clusters
// that no longer have an endpoint of their own (or of a deleted storage, when
// stored is nil). A changed SMB password render alone is not a change.
func (g *Gateway) syncStorageEndpointSecrets(ctx context.Context, id string, stored *storedStorageClass, identity *v1.Secret) error {
	namespace := g.managementNamespace()
	existing, err := g.rawClient.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set{storageconfig.EndpointLabel: id}.AsSelector().String(),
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
		if desired, err = storageconfig.RenderEndpoints(namespace, id, *stored); err != nil {
			return err
		}
	}
	for _, secret := range desired {
		old, found := current[secret.Name]
		delete(current, secret.Name)
		if !found {
			tenant.SetIdentity(secret, tenant.IDOf(identity), identity.Annotations[tenant.CreatedByAnnotation])
			if _, err := g.rawClient.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
				return fmt.Errorf("create storage endpoint declaration %s: %w", secret.Name, err)
			}
			continue
		}
		if storageconfig.RenderKey(old.Data) == storageconfig.RenderKey(secret.Data) {
			continue
		}
		old.Data = secret.Data
		if _, err := g.rawClient.CoreV1().Secrets(namespace).Update(ctx, old, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("update storage endpoint declaration %s: %w", old.Name, err)
		}
	}
	for name := range current {
		if err := g.rawClient.CoreV1().Secrets(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete storage endpoint declaration %s: %w", name, err)
		}
	}
	return nil
}
