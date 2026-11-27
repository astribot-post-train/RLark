package stations

import (
	"context"
	"errors"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

var ErrEnrollmentChanged = errors.New("the enrollment changed or no longer has a Node UID conflict; refresh the record")

// HasNodeUIDConflict distinguishes stale enrollment identity from other kinds
// of conflict the Station controller may report.
func HasNodeUIDConflict(record *unstructured.Unstructured) bool {
	state, _, _ := unstructured.NestedString(record.Object, "status", "node", "state")
	conditions, _, _ := unstructured.NestedSlice(record.Object, "status", "node", "conditions")
	return state == "Conflict" && slices.ContainsFunc(conditions, func(raw any) bool {
		condition, _ := raw.(map[string]any)
		return condition["type"] == "IdentityMatched" && condition["status"] == "False" &&
			condition["reason"] == "NodeUIDChanged"
	})
}

// ForgetConflictingEnrollment owns only the original NetworkEnrollment. It
// has no Node or NodeOperation client, even if another Node uses the name.
func ForgetConflictingEnrollment(ctx context.Context, resource dynamic.ResourceInterface, name string, expected types.UID) error {
	if expected == "" {
		return ErrEnrollmentChanged
	}
	record, err := resource.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	version := record.GetResourceVersion()
	bound, _, _ := unstructured.NestedString(record.Object, "spec", "nodeUID")
	reported, _, _ := unstructured.NestedString(record.Object, "status", "observation", "nodeUID")
	if record.GetUID() != expected || version == "" || !HasNodeUIDConflict(record) ||
		bound == "" || reported == "" || bound == reported {
		return ErrEnrollmentChanged
	}
	err = resource.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &expected, ResourceVersion: &version}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if apierrors.IsConflict(err) {
		return ErrEnrollmentChanged
	}
	return err
}
