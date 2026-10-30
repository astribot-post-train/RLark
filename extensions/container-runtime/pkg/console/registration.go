package console

import (
	"net/http"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/provision"
	"github.com/rlinf/rlark/apps/rlark/pkg/log"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
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
