package stationidentity

import (
	"encoding/json"
	"log"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
)

// nodeLinkLimit bounds one record: ten actions with their results take 2-3 KiB, and all of a Node's annotations
// together may not exceed 256 KiB.
const nodeLinkLimit = 8 << 10

type nodeLinkRequest struct {
	documentRequest
	Link nodeLink `json:"link"`
}

// nodeLink is the station-gateway's link.json without rlark (when Station API last answered, which the gateway leaves
// out): the tunnel's state since when, what owns the machine's restart, the steps of this outage, the restart asked for last and the last actions.
// Times are Unix seconds of the node's clock.
type nodeLink struct {
	State     string `json:"state"`
	RLarkLost bool   `json:"rlarkLost,omitempty"`
	Since     int64  `json:"since"`
	Uplink    string `json:"uplink,omitempty"`
	// RestartOwner is what restarts the machine when its tunnel stays down ("": a person).
	RestartOwner string           `json:"restartOwner,omitempty"`
	Done         []string         `json:"done,omitempty"`
	Restarts     int              `json:"restarts"`
	Restart      *nodeLinkRestart `json:"restart,omitempty"`
	Actions      []nodeLinkAction `json:"actions"`
}

type nodeLinkRestart struct {
	ID  int    `json:"id"`
	At  int64  `json:"at"`
	Why string `json:"why"`
}

type nodeLinkAction struct {
	At     int64  `json:"at"`
	Step   string `json:"step"`
	Result string `json:"result"`
}

// ServeLink takes an enrolled node's link record for its WireGuard key and keeps it on the node's RLark Node
// (annotation rlark.io/link), so every node kind's record reaches RLark the one way every node asks Station API.
func (d *NetworkDocuments) ServeLink(w http.ResponseWriter, r *http.Request) {
	var request nodeLinkRequest
	if !decodeBody(w, r, &request) || !d.proven(w, request.documentRequest) {
		return
	}
	record, _ := json.Marshal(request.Link)
	if request.Link.State != "up" && request.Link.State != "down" || len(request.Link.Actions) > 10 || len(record) > nodeLinkLimit {
		writeError(w, http.StatusBadRequest, "invalid link record")
		return
	}
	holder, err := d.holder(r.Context(), request.PublicKey)
	if err != nil {
		log.Printf("node link: %v", err)
		writeError(w, http.StatusServiceUnavailable, "network document storage unavailable")
		return
	}
	// An admitted machine that has not enrolled has no RLark Node yet.
	if holder == nil || holder.GetKind() != "NetworkEnrollment" {
		writeError(w, http.StatusNotFound, "no enrolled node for this key")
		return
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{rlarkv1alpha1.NodeLinkAnnotation: string(record)}}})
	_, err = d.Client.Resource(rlarkNodeGVR).Namespace(holder.GetNamespace()).Patch(r.Context(), holder.GetName(), types.MergePatchType, patch, metav1.PatchOptions{})
	switch {
	case apierrors.IsNotFound(err):
		writeError(w, http.StatusNotFound, "no RLark Node of this name")
	case err != nil:
		log.Printf("node link of %s/%s: %v", holder.GetNamespace(), holder.GetName(), err)
		writeError(w, http.StatusServiceUnavailable, "RLark Node unavailable")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
