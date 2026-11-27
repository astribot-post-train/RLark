package release

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

const stockRBACOwner = "rlark.io/stock-rbac-cluster-uid"

type stockRBACChange struct {
	object *unstructured.Unstructured
	create bool
}

// stockRBACChanges is shared by plan and apply. A matching manual definition
// can satisfy the policy, but only release-owned objects are reconciled.
func stockRBACChanges(ctx context.Context, plane dynamic.Interface, definitions []*unstructured.Unstructured, clusterUID types.UID) ([]stockRBACChange, error) {
	var changes []stockRBACChange
	for _, definition := range definitions {
		client := definitionClient(plane, definition)
		current, err := client.Get(ctx, definition.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			next := definition.DeepCopy()
			next.SetAnnotations(map[string]string{stockRBACOwner: string(clusterUID)})
			changes = append(changes, stockRBACChange{object: next, create: true})
			continue
		}
		if err != nil {
			return nil, err
		}
		owner := current.GetAnnotations()[stockRBACOwner]
		if current.GetDeletionTimestamp() != nil || (owner != "" && owner != string(clusterUID)) {
			return nil, fmt.Errorf("stock executor %s has a different owner or is being deleted", definitionName(definition))
		}
		if len(differences(jsonValue(current.Object), jsonValue(stated(definition)), false)) == 0 {
			continue
		}
		if owner == "" {
			return nil, &stockDeferredError{reason: fmt.Sprintf("manually managed stock executor %s differs from the release definition", definitionName(definition))}
		}
		// A binding's roleRef cannot be edited in place. A role migration must
		// specify a new binding name so this action never deletes an old binding.
		if current.GetKind() == "RoleBinding" &&
			len(differences(jsonValue(current.Object["roleRef"]), jsonValue(definition.Object["roleRef"]), false)) != 0 {
			return nil, fmt.Errorf("stock executor %s changed its immutable roleRef; use a new binding name", definitionName(definition))
		}
		next := current.DeepCopy()
		for field, value := range stated(definition) {
			if err := unstructured.SetNestedField(next.Object, value, field); err != nil {
				return nil, err
			}
		}
		changes = append(changes, stockRBACChange{object: next})
	}
	return changes, nil
}
