// Package metadata defines shared RLark resource identity metadata.
package metadata

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	DefaultID                 = "default"
	IDLabel                   = "rlark.io/tenant-id"
	ControllerOwnerAnnotation = "rlark.io/controller-owner"
	// TargetClusterUIDAnnotation pins a Task to one management Cluster instance.
	TargetClusterUIDAnnotation = "rlark.io/target-cluster-uid"
)

// IDOf preserves the legacy default for resources without tenant metadata.
func IDOf(object metav1.Object) string {
	if object != nil && object.GetLabels()[IDLabel] != "" {
		return object.GetLabels()[IDLabel]
	}
	return DefaultID
}
