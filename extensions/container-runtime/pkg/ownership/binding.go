// Package ownership contains the small, runtime-neutral ownership checks used
// by the container extension. It deliberately has no controller or provider
// side effects so it can be shared by API and reconciliation paths.
package ownership

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	containerapi "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
)

// MatchesNative proves that a companion reference still points at the exact
// Kubernetes object instance. Names and labels alone are never sufficient.
func MatchesNative(ref containerapi.NativeObjectReference, obj metav1.Object) error {
	if obj == nil {
		return fmt.Errorf("native object is required")
	}
	if ref.Namespace != obj.GetNamespace() || ref.Name != obj.GetName() {
		return fmt.Errorf("native reference %s/%s does not match %s/%s", ref.Namespace, ref.Name, obj.GetNamespace(), obj.GetName())
	}
	if ref.UID == "" || ref.UID != string(obj.GetUID()) {
		return fmt.Errorf("native reference UID does not match %s/%s", obj.GetNamespace(), obj.GetName())
	}
	return nil
}

// JobBindingMatches verifies the extension binding before it can claim a Job.
func JobBindingMatches(binding *containerapi.ContainerJob, job metav1.Object, tenantID string) error {
	if binding == nil {
		return fmt.Errorf("container Job binding is required")
	}
	if err := MatchesNative(binding.Spec.Job, job); err != nil {
		return err
	}
	if binding.Spec.TenantID == "" || binding.Spec.TenantID != tenantID {
		return fmt.Errorf("container Job tenant does not match")
	}
	if binding.Spec.Runtime == "" {
		return fmt.Errorf("container Job runtime is required")
	}
	return nil
}
