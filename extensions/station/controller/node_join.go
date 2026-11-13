package controller

import (
	"context"
	"errors"
	"fmt"

	stationv1alpha1 "github.com/rlinf/rlark/extensions/station/api/v1alpha1"
)

// NodeJoinState is observed from the target host and member API together.
// VerifiedUID is set only after the kubelet certificate and member Node have
// both been matched to the operation's node identity.
type NodeJoinState struct {
	Attempted             bool
	ExistingConfiguration bool
	VerifiedUID           string
	// Pending means the operation-owned attempt is still being observed, such
	// as while its kubelet CSR or Node registration is being created.
	Pending bool
}

// NodeJoinDriver implements operation-owned marker checks, certificate and
// Node UID verification, and the actual remote kubeadm invocation.
type NodeJoinDriver interface {
	Observe(context.Context, *stationv1alpha1.NodeOperation) (NodeJoinState, error)
	Start(context.Context, *stationv1alpha1.NodeOperation) error
}

var ErrJoinOutcomeUnknown = errors.New("join attempt exists but node identity cannot be verified")
var ErrNodeAlreadyConfigured = errors.New("node already has Kubernetes configuration")

// ExecuteNodeJoin performs at most one remote join attempt. An uncertain
// result is observed again rather than replaying a non-repeatable command.
func ExecuteNodeJoin(ctx context.Context, driver NodeJoinDriver, op *stationv1alpha1.NodeOperation) (bool, string, error) {
	if driver == nil || op == nil || op.UID == "" || op.Spec.Action != "Join" || op.Spec.NodeName == "" {
		return false, "", errors.New("join driver and operation identity are required")
	}
	state, err := driver.Observe(ctx, op)
	if err != nil {
		return false, "", fmt.Errorf("observe join state: %w", err)
	}
	if state.VerifiedUID != "" {
		if !state.Attempted {
			return false, "", ErrNodeAlreadyConfigured
		}
		return true, state.VerifiedUID, nil
	}
	if state.Attempted {
		if state.Pending {
			return false, "", nil
		}
		return false, "", ErrJoinOutcomeUnknown
	}
	if state.ExistingConfiguration {
		return false, "", ErrNodeAlreadyConfigured
	}
	if err := driver.Start(ctx, op); err != nil {
		return false, "", fmt.Errorf("start join attempt: %w", err)
	}
	// Wait for a subsequent observation; command success is not identity proof.
	return false, "", nil
}
