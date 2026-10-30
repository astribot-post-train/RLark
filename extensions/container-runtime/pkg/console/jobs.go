package console

import (
	"errors"
	"net/http"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/submission"
	"github.com/rlinf/rlark/apps/rlark/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func clusterTarget(namespace, name string, uid string) submission.ClusterTarget {
	return submission.ClusterTarget{Namespace: namespace, Name: name, UID: uid}
}

func submissionError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, submission.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid Raw Job submission")
		return true
	case errors.Is(err, submission.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "Job identity or desired state changed; refresh")
		return true
	default:
		return false
	}
}

func (a *API) submitJob(w http.ResponseWriter, r *http.Request, namespace, name, subject string) {
	var req submission.Request
	if !decodeStrict(w, r, &req) {
		return
	}
	// Cluster UID is supplied in the request body only as a precondition. The
	// submission service re-reads and validates the companion/native binding.
	if req.ClusterUID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "cluster UID is required")
		return
	}
	result, err := a.Jobs.Submit(r.Context(), clusterTarget(namespace, name, req.ClusterUID), req)
	if err != nil {
		if submissionError(w, err) {
			return
		}
		writeKubeError(w, err)
		return
	}
	log.FromContext(r.Context()).Info("container Raw Job submission observed", "subject", subject, "operationID", result.OperationID, "job", result.Name, "uid", result.UID)
	writeJSON(w, http.StatusCreated, result)
}

func (a *API) listJobs(w http.ResponseWriter, r *http.Request, namespace, name string) {
	// Listing needs the current companion UID. It is intentionally not inferred
	// from names or labels supplied by the caller.
	var cc ext.ContainerCluster
	if err := a.Client.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: name}, &cc); err != nil {
		writeKubeError(w, err)
		return
	}
	items, err := a.Jobs.List(r.Context(), clusterTarget(namespace, name, string(cc.UID)))
	if err != nil {
		if submissionError(w, err) {
			return
		}
		writeKubeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Items []submission.Summary `json:"items"`
	}{items})
}

type suspendJobRequest struct {
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion"`
}

func (a *API) suspendJob(w http.ResponseWriter, r *http.Request, namespace, name, jobName, subject string) {
	var req suspendJobRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.UID == "" || req.ResourceVersion == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "exact Job UID and resourceVersion are required")
		return
	}
	var cc ext.ContainerCluster
	if err := a.Client.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: name}, &cc); err != nil {
		writeKubeError(w, err)
		return
	}
	result, err := a.Jobs.Suspend(r.Context(), clusterTarget(namespace, name, string(cc.UID)), jobName, req.UID, req.ResourceVersion)
	if err != nil {
		if submissionError(w, err) {
			return
		}
		writeKubeError(w, err)
		return
	}
	log.FromContext(r.Context()).Info("container Raw Job suspension observed", "subject", subject, "job", result.Name, "uid", result.UID)
	writeJSON(w, http.StatusOK, result)
}
