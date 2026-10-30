package console

import (
	"encoding/json"
	"net/http"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/companion"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/ownership"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/provision"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/log"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type registerNodeRequest struct {
	OperationID        string   `json:"operationID"`
	ClusterUID         string   `json:"clusterUID"`
	Name               string   `json:"name"`
	InstanceID         string   `json:"instanceID"`
	PrivateAddress     string   `json:"privateAddress"`
	Environment        string   `json:"environment"`
	NetworkScope       string   `json:"networkScope"`
	AuthorizedGPUUUIDs []string `json:"authorizedGPUUUIDs"`
}

func (a *API) registerNode(w http.ResponseWriter, r *http.Request, namespace, clusterName, subject string) {
	var body registerNodeRequest
	if !decodeStrict(w, r, &body) {
		return
	}
	if a.Registrar == nil {
		writeError(w, http.StatusNotFound, "unsupported", "node registration is disabled")
		return
	}
	if body.ClusterUID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "cluster UID is required")
		return
	}
	var cc ext.ContainerCluster
	if err := a.Client.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: clusterName}, &cc); err != nil {
		writeKubeError(w, err)
		return
	}
	nativeCluster, valid, err := a.binding(r.Context(), &cc)
	if err != nil {
		writeKubeError(w, err)
		return
	}
	if !valid || nativeCluster == nil || cc.Spec.Runtime != "Raw" || cc.DeletionTimestamp != nil || nativeCluster.DeletionTimestamp != nil {
		writeError(w, http.StatusConflict, "conflict", "cluster binding is invalid; refresh before retrying")
		return
	}
	if body.ClusterUID != string(cc.UID) {
		writeError(w, http.StatusConflict, "conflict", "cluster identity changed; refresh before retrying")
		return
	}
	req := provision.NodeRequest{
		OperationID: body.OperationID, Cluster: nativeCluster.Name, ClusterUID: string(nativeCluster.UID),
		ContainerClusterUID: string(cc.UID), TenantID: tenant.IDOf(&cc), Name: body.Name,
		InstanceID: body.InstanceID, PrivateAddress: body.PrivateAddress, Environment: body.Environment,
		NetworkScope: body.NetworkScope, AuthorizedGPUUUIDs: body.AuthorizedGPUUUIDs,
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	node, err := a.Registrar.RegisterNode(r.Context(), req)
	if err != nil {
		writeKubeError(w, err)
		return
	}
	if node == nil || node.UID == "" || node.Name != req.Name || node.Namespace != namespace || node.Spec.Cluster != cc.Spec.Cluster || node.Spec.Node.UID == "" || node.Spec.Node.Namespace != namespace || node.Spec.Node.Name != req.Name || tenant.IDOf(node) != req.TenantID || node.Annotations[ext.AgentCredentialAnnotation] != req.OperationID {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "node registration result could not be verified")
		return
	}
	log.FromContext(r.Context()).Info("container node registration observed", "subject", subject, "operationID", req.OperationID, "cluster", cc.Name, "node", node.Name, "uid", node.UID)
	writeJSON(w, http.StatusCreated, struct {
		Namespace   string `json:"namespace"`
		Name        string `json:"name"`
		UID         string `json:"uid"`
		NativeUID   string `json:"nativeUID"`
		ClusterUID  string `json:"clusterUID"`
		OperationID string `json:"operationID"`
	}{node.Namespace, node.Name, string(node.UID), string(node.Spec.Node.UID), string(cc.UID), req.OperationID})
}

type patchNodeSchedulingRequest struct {
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion"`
	Unschedulable   *bool  `json:"unschedulable"`
}

// patchNodeScheduling changes the native durable scheduling intent. Cordon is
// safe with a running workload; uncordon requires every known allocation and
// legacy native reservation to be gone. Drain is deliberately not represented
// by this endpoint because it needs its own recoverable operation resource.
func (a *API) patchNodeScheduling(w http.ResponseWriter, r *http.Request, namespace, clusterName, nodeName, subject string) {
	var req patchNodeSchedulingRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.UID == "" || req.ResourceVersion == "" || req.Unschedulable == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "exact node identity and unschedulable are required")
		return
	}
	var cc ext.ContainerCluster
	if err := a.Client.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: clusterName}, &cc); err != nil {
		writeKubeError(w, err)
		return
	}
	nativeCluster, valid, err := a.binding(r.Context(), &cc)
	if err != nil {
		writeKubeError(w, err)
		return
	}
	if !valid || nativeCluster == nil || cc.UID == "" || !cc.DeletionTimestamp.IsZero() || !nativeCluster.DeletionTimestamp.IsZero() {
		writeError(w, http.StatusConflict, "conflict", "cluster binding is invalid; refresh before retrying")
		return
	}
	var cn ext.ContainerNode
	if err := a.Client.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: nodeName}, &cn); err != nil {
		writeKubeError(w, err)
		return
	}
	if string(cn.UID) != req.UID || cn.Spec.Cluster != cc.Spec.Cluster || cn.Spec.Runtime != cc.Spec.Runtime || cn.Spec.Node.Namespace != namespace || cn.Spec.Node.Name == "" || cn.Spec.Node.UID == "" || tenant.IDOf(&cn) != tenant.IDOf(&cc) || !cn.DeletionTimestamp.IsZero() {
		writeError(w, http.StatusConflict, "conflict", "node binding changed; refresh before retrying")
		return
	}
	var nn native.Node
	if err := a.Client.Get(r.Context(), client.ObjectKey{Namespace: cn.Spec.Node.Namespace, Name: cn.Spec.Node.Name}, &nn); err != nil {
		writeKubeError(w, err)
		return
	}
	runtimeMatches := (cn.Spec.Runtime == "Raw" && nn.Spec.AgentType == native.AgentTypeRaw) || (cn.Spec.Runtime == "Docker" && nn.Spec.AgentType == native.AgentTypeDocker)
	if !runtimeMatches || ownership.MatchesNative(cn.Spec.Node, &nn) != nil || tenant.IDOf(&nn) != tenant.IDOf(&cc) || nn.DeletionTimestamp != nil || nn.ResourceVersion != req.ResourceVersion {
		writeError(w, http.StatusConflict, "conflict", "node changed or binding is invalid; refresh before retrying")
		return
	}
	if !*req.Unschedulable {
		var allocations ext.ContainerAllocationList
		if err := a.Client.List(r.Context(), &allocations, client.InNamespace(namespace)); err != nil {
			writeKubeError(w, err)
			return
		}
		for _, allocation := range allocations.Items {
			if allocation.Name == companion.Name(string(nn.UID)) || allocation.Spec.NodeRecordUID == string(cn.UID) || allocation.Spec.Node.UID == string(nn.UID) || (allocation.Spec.Node.Name == nn.Name && allocation.Spec.Node.Namespace == nn.Namespace) {
				writeError(w, http.StatusConflict, "node_held", "node has an active container allocation")
				return
			}
		}
		if nn.Status.Reservation != nil || len(nn.Status.DockerReservations) != 0 {
			writeError(w, http.StatusConflict, "node_held", "node has a legacy native reservation")
			return
		}
	}
	// Test both incarnation and version atomically with the desired-state write.
	// A replaced object must never inherit an administrator's stale action.
	patch, err := json.Marshal([]struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value any    `json:"value"`
	}{
		{"test", "/metadata/uid", string(nn.UID)},
		{"test", "/metadata/resourceVersion", req.ResourceVersion},
		{"add", "/spec/unschedulable", *req.Unschedulable},
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "unable to encode node intent")
		return
	}
	if err := a.Client.Patch(r.Context(), &nn, client.RawPatch(types.JSONPatchType, patch)); err != nil {
		if apierrors.IsInvalid(err) {
			writeError(w, http.StatusConflict, "conflict", "node changed; refresh before retrying")
			return
		}
		writeKubeError(w, err)
		return
	}
	log.FromContext(r.Context()).Info("container node scheduling intent updated", "subject", subject, "cluster", cc.Name, "node", cn.Name, "unschedulable", *req.Unschedulable)
	writeJSON(w, http.StatusOK, struct {
		UID             string `json:"uid"`
		ResourceVersion string `json:"resourceVersion"`
		Unschedulable   bool   `json:"unschedulable"`
	}{string(nn.UID), nn.ResourceVersion, nn.Spec.Unschedulable})
}
