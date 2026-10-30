package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestTypesRegisterAndDeepCopy(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	obj := &ContainerNode{ObjectMeta: metav1.ObjectMeta{Name: "node"}, Status: ContainerNodeStatus{Capacity: map[string]string{"gpu": "1"}}}
	copy := obj.DeepCopy()
	copy.Status.Capacity["gpu"] = "0"
	if obj.Status.Capacity["gpu"] != "1" {
		t.Fatal("deep copy aliased capacity")
	}
	if _, err := scheme.New(GroupVersion.WithKind("ContainerNode")); err != nil {
		t.Fatal(err)
	}
}
