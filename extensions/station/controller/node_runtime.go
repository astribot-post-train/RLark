package controller

import (
	"errors"

	stationv1alpha1 "github.com/rlinf/rlark/extensions/station/api/v1alpha1"
)

const (
	nodeContainerdLabel = "rlark.io/containerd"
	nodeClusterIDLabel  = "rlark.io/cluster-id"
	nodeEnabledLabel    = "rlark.io/enabled"
)

func containerdSocketForRuntime(runtime string) (string, error) {
	switch runtime {
	case "", "containerd":
		return "/run/containerd/containerd.sock", nil
	case "fleet":
		return "/run/containerd-fleet/containerd.sock", nil
	default:
		return "", errors.New("unsupported containerd runtime")
	}
}

func nodeOperationRuntime(op *stationv1alpha1.NodeOperation) (string, error) {
	if op == nil || op.Spec.Action != "Join" {
		return "", errors.New("Join operation is required")
	}
	runtime := op.Spec.ContainerdRuntime
	if runtime == "" {
		runtime = "containerd"
	}
	if _, err := containerdSocketForRuntime(runtime); err != nil {
		return "", err
	}
	return runtime, nil
}
