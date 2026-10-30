package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/companion"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// cleanupReceiptAnnotation is a controller-owned recovery receipt, never Agent
// authority. It is copied from exact allocation cleanup proof before release so
// a crash after allocation deletion does not lose that observation.
const cleanupReceiptAnnotation = "containers.rlark.io/allocation-cleanup-receipt"

type cleanupReceipt struct {
	Allocation ext.NativeObjectReference   `json:"allocation"`
	Target     ext.ContainerAllocationSpec `json:"target"`
}

func readReceipt(task *native.Task, target *ext.ContainerAllocationSpec) (*cleanupReceipt, error) {
	data, exists := task.Annotations[cleanupReceiptAnnotation]
	if !exists {
		return nil, nil
	}
	d := json.NewDecoder(bytes.NewBufferString(data))
	d.DisallowUnknownFields()
	var receipt cleanupReceipt
	if err := d.Decode(&receipt); err != nil {
		return nil, invalidBinding("invalid allocation cleanup receipt")
	}
	if err := d.Decode(new(any)); err != io.EOF || target == nil || !reflect.DeepEqual(receipt.Target, *target) || receipt.Allocation.UID == "" || receipt.Allocation.Namespace != task.Namespace || receipt.Allocation.Name != companion.Name(target.Node.UID) {
		return nil, invalidBinding("allocation cleanup receipt does not match Task target")
	}
	return &receipt, nil
}

func cleanupProven(a *ext.ContainerAllocation) bool {
	proof := meta.FindStatusCondition(a.Status.Conditions, "CleanupConfirmed")
	return a.Status.CleanupConfirmed && a.Status.ObservedGeneration == a.Generation && proof != nil && proof.Status == metav1.ConditionTrue && proof.ObservedGeneration == a.Generation
}

func (r JobReservationReconciler) saveReceipt(ctx context.Context, task *native.Task, a *ext.ContainerAllocation) error {
	receipt := cleanupReceipt{Allocation: ext.NativeObjectReference{Namespace: a.Namespace, Name: a.Name, UID: string(a.UID)}, Target: a.Spec}
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if task.Annotations[cleanupReceiptAnnotation] == string(data) {
		return nil
	}
	before := task.DeepCopy()
	task.Annotations[cleanupReceiptAnnotation] = string(data)
	return r.Client.Patch(ctx, task, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}
