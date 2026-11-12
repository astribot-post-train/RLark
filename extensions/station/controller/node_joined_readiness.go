package controller

import (
	"context"
	"errors"
	"fmt"

	stationv1alpha1 "github.com/rlinf/rlark/extensions/station/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var ErrStationRebindRequired = errors.New("Station must be explicitly rebound to the new Kubernetes Node UID")

// StationJoinedReadiness combines the verified Station binding with the
// management Node condition. A same-named RLark Node alone is not identity.
type StationJoinedReadiness struct {
	Reader client.Reader
	Nodes  NodeReadiness
}

func (r StationJoinedReadiness) Ready(ctx context.Context, clusterID, namespace, nodeName, nodeUID string) (bool, error) {
	if r.Reader == nil || r.Nodes == nil || clusterID == "" || namespace == "" || nodeName == "" || nodeUID == "" {
		return false, errors.New("joined node readiness dependencies and identity are required")
	}
	list := &stationv1alpha1.StationList{}
	if err := r.Reader.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return false, fmt.Errorf("list station bindings: %w", err)
	}
	var found *stationv1alpha1.Station
	for i := range list.Items {
		station := &list.Items[i]
		if station.Spec.Binding.NodeName != nodeName {
			continue
		}
		if found != nil {
			return false, errors.New("multiple Stations claim joined node")
		}
		found = station
	}
	if found == nil {
		return false, nil
	}
	if found.Spec.Binding.NodeUID != nodeUID {
		return false, ErrStationRebindRequired
	}
	if found.UID == "" || found.Spec.Retired || found.Status.Phase == stationv1alpha1.StationPhaseConflict || found.Status.Phase == stationv1alpha1.StationPhaseRetired {
		return false, ErrNodeIdentityChanged
	}
	if found.Status.Phase != stationv1alpha1.StationPhaseBound {
		return false, nil
	}
	return r.Nodes.Ready(ctx, clusterID, namespace, nodeName)
}
