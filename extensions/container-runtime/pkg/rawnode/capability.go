package rawnode

import (
	"encoding/json"
	"errors"
	"net"
	"slices"
	"strings"

	api "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
)

// CapabilityAnnotation is administrator-owned authorization for the existing
// container. An Agent report must match it exactly; visibility grants nothing.
const CapabilityAnnotation = "raw.rlark.io/authorized-capability"

// ValidateCapability checks the explicit private network, environment and GPU scope.
func ValidateCapability(v *api.RawNodeStatus) error {
	if v == nil || v.Incarnation == "" || strings.TrimSpace(v.Environment) == "" || strings.TrimSpace(v.NetworkScope) == "" || len(v.AuthorizedGPUUUIDs) == 0 {
		return errors.New("complete Raw incarnation, environment, private network and authorized GPUs required")
	}
	ip := net.ParseIP(v.TrainingIP)
	if ip == nil || !ip.IsPrivate() || ip.To4() == nil {
		return errors.New("Raw training address must be a private IPv4 address")
	}
	seen := map[string]bool{}
	for _, id := range v.AuthorizedGPUUUIDs {
		if !strings.HasPrefix(id, "GPU-") || strings.ContainsAny(id, ", \t\r\n") || seen[id] {
			return errors.New("unique explicit GPU UUIDs required")
		}
		seen[id] = true
	}
	return nil
}

// AuthorizedCapability reads the trusted provisioned scope, never observed devices.
func AuthorizedCapability(node *api.Node) (*api.RawNodeStatus, error) {
	var v api.RawNodeStatus
	if node == nil || json.Unmarshal([]byte(node.Annotations[CapabilityAnnotation]), &v) != nil {
		return nil, errors.New("administrator-authorized Raw capability required")
	}
	if err := ValidateCapability(&v); err != nil {
		return nil, err
	}
	return &v, nil
}

// SameCapability compares GPU sets independently of device enumeration order.
func SameCapability(a, b *api.RawNodeStatus) bool {
	if a == nil || b == nil {
		return a == b
	}
	x, y := *a, *b
	x.AuthorizedGPUUUIDs, y.AuthorizedGPUUUIDs = nil, nil
	return x.Incarnation == y.Incarnation && x.TrainingIP == y.TrainingIP && x.NetworkScope == y.NetworkScope && x.Environment == y.Environment && len(a.AuthorizedGPUUUIDs) == len(b.AuthorizedGPUUUIDs) && slices.Equal(sorted(a.AuthorizedGPUUUIDs), sorted(b.AuthorizedGPUUUIDs))
}
func sorted(v []string) []string { result := slices.Clone(v); slices.Sort(result); return result }
