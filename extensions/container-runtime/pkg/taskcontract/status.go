package taskcontract

import (
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Observed checks the current instance's Spec-driven execution observation.
func Observed(t *native.Task) bool {
	return t != nil && t.UID != "" && t.Generation > 0 && t.Status.ObservedGeneration == t.Generation && currentTrue(t, "ExecutionKnown")
}

// Completed requires both a current result and supervised descendant cleanup.
// It does not prove durable cancellation; allocation release needs the separate
// supervisor Stop/cleanup handshake even after natural process completion.
func Completed(t *native.Task) bool {
	return Observed(t) && (t.Status.Phase == native.TaskPhaseSucceeded || t.Status.Phase == native.TaskPhaseFailed) && currentTrue(t, "ExecutionCleaned")
}

// HasResult identifies phases that must never fall through to a new start,
// including stale or incomplete observations that need explicit recovery.
func HasResult(t *native.Task) bool {
	return t != nil && (t.Status.Phase == native.TaskPhaseSucceeded || t.Status.Phase == native.TaskPhaseFailed || t.Status.Phase == native.TaskPhaseStopped)
}

func currentTrue(t *native.Task, kind string) bool {
	c := meta.FindStatusCondition(t.Status.Conditions, kind)
	return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == t.Generation
}
