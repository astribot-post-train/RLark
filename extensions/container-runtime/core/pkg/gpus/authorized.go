// Package gpus resolves the GPU set a container Node may present to scheduling.
package gpus

import (
	"errors"
	"slices"
	"strings"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/core/api/v1alpha1"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const heartbeatFreshness = 45 * time.Second

var ErrUnavailable = errors.New("authorized GPU observation unavailable")

// Authorized returns the immutable explicit allowlist when present. An empty
// allowlist opts into the latest authenticated visible-device observation and
// fails closed when that heartbeat is absent, future-dated or stale.
func Authorized(node *ext.ContainerNode, now time.Time) ([]string, error) {
	if node == nil {
		return nil, ErrUnavailable
	}
	values := node.Spec.AuthorizedGPUUUIDs
	if len(values) == 0 {
		at := node.Status.LastHeartbeatTime
		credential := node.Annotations[ext.AgentCredentialAnnotation]
		ready := meta.FindStatusCondition(node.Status.Conditions, "Ready")
		if credential == "" || credential != node.Status.AgentCredentialID || node.Status.ObservedGeneration != node.Generation || ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != node.Generation || at == nil || at.After(now) || now.Sub(at.Time) > heartbeatFreshness {
			return nil, ErrUnavailable
		}
		values = node.Status.DiscoveredGPUUUIDs
	}
	if len(values) == 0 {
		return nil, ErrUnavailable
	}
	result := slices.Clone(values)
	slices.Sort(result)
	for i, value := range result {
		if !strings.HasPrefix(value, "GPU-") || strings.ContainsAny(value, ", \t\r\n") || i > 0 && result[i-1] == value {
			return nil, ErrUnavailable
		}
	}
	return result, nil
}

// Inventory returns the validated retained inventory for accounting and cleanup.
// It does not grant scheduling authority; callers starting work must check Authorized.
func Inventory(node *ext.ContainerNode) []string {
	if node == nil {
		return nil
	}
	if len(node.Spec.AuthorizedGPUUUIDs) != 0 {
		values, err := Authorized(node, time.Time{})
		if err != nil {
			return nil
		}
		return values
	}
	copy := node.DeepCopy()
	copy.Spec.AuthorizedGPUUUIDs = slices.Clone(node.Status.DiscoveredGPUUUIDs)
	if len(copy.Spec.AuthorizedGPUUUIDs) == 0 {
		return nil
	}
	values, err := Authorized(copy, time.Time{})
	if err != nil {
		return nil
	}
	return values
}
