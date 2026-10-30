package v1alpha1

import (
	"encoding/json"
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

func TestUpgradeRegistrationAndAllocationCompatibility(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"AgentUpgrade", "AgentUpgradeList"} {
		if _, err := scheme.New(GroupVersion.WithKind(kind)); err != nil {
			t.Fatal(err)
		}
	}
	allocation := &ContainerAllocation{Spec: ContainerAllocationSpec{Upgrade: &UpgradeAllocation{Operation: NativeObjectReference{Name: "op", UID: "uid"}}}}
	copy := allocation.DeepCopy()
	copy.Spec.Upgrade.Operation.UID = "other"
	if allocation.Spec.Upgrade.Operation.UID != "uid" {
		t.Fatal("upgrade binding aliases copy")
	}
	data, err := json.Marshal(allocation.Spec)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["upgrade"] == nil || wire["job"] != nil || wire["task"] != nil || wire["executionID"] != nil {
		t.Fatalf("upgrade carries workload identity: %s", data)
	}
	legacy := []byte(`{"cluster":{"name":"cloud","uid":"cluster"},"job":{"name":"job","uid":"job-uid"},"task":{"name":"task","uid":"task-uid"},"node":{"name":"node","uid":"node-uid"},"tenantID":"default","generation":1,"jobGeneration":1,"executionID":"task-uid","jobBindingUID":"binding","nodeInstanceID":"instance","privateAddress":"10.0.3.85","networkScope":"vpc","nodeRecordUID":"record"}`)
	var restored ContainerAllocationSpec
	if err := json.Unmarshal(legacy, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Upgrade != nil || restored.Task.UID != "task-uid" || restored.Generation != 1 {
		t.Fatalf("legacy workload misclassified: %#v", restored)
	}
	op := &AgentUpgrade{Status: AgentUpgradeStatus{Allocation: &NativeObjectReference{UID: "allocation"}, StartedAt: &metav1.Time{}}}
	other := op.DeepCopy()
	other.Status.Allocation.UID = "foreign"
	other.Status.StartedAt.Time = metav1.Now().Time
	if op.Status.Allocation.UID != "allocation" || !op.Status.StartedAt.IsZero() {
		t.Fatal("upgrade status aliases copy")
	}
}
