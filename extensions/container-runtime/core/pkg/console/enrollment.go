package console

import (
	"context"
	"log/slog"
	"net/http"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/core/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/core/pkg/provision"
	tenant "github.com/rlinf/rlark/api/metadata"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type enrollmentSummary struct {
	Name            string                     `json:"name"`
	UID             string                     `json:"uid"`
	OperationID     string                     `json:"operationID"`
	ReleaseName     string                     `json:"releaseName"`
	ReleaseVersion  string                     `json:"releaseVersion"`
	Phase           string                     `json:"phase"`
	CancelRequested bool                       `json:"cancelRequested"`
	Targets         []ext.EnrollmentNode       `json:"targets"`
	Nodes           []ext.EnrollmentNodeStatus `json:"nodes"`
	Conditions      []metav1.Condition         `json:"conditions"`
}

func summarizeEnrollment(op ext.NodeEnrollment) enrollmentSummary {
	return enrollmentSummary{Name: op.Name, UID: string(op.UID), OperationID: op.Spec.OperationID, ReleaseName: op.Spec.Release.Name, ReleaseVersion: op.Spec.ReleaseSpec.Version, Phase: op.Status.Phase, CancelRequested: op.Spec.CancelRequested, Targets: op.Spec.Nodes, Nodes: op.Status.Nodes, Conditions: op.Status.Conditions}
}

func (a *API) cancelEnrollment(w http.ResponseWriter, r *http.Request, namespace, clusterName, name, subject string) {
	if a.Enrollments == nil {
		writeError(w, 404, "unsupported", "Notebook enrollment disabled")
		return
	}
	var body struct {
		UID        string `json:"uid"`
		ClusterUID string `json:"clusterUID"`
	}
	if !decodeStrict(w, r, &body) {
		return
	}
	if body.UID == "" || body.ClusterUID == "" {
		writeError(w, 400, "invalid_request", "operation and cluster UIDs required")
		return
	}
	var cc ext.ContainerCluster
	if err := a.Client.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: clusterName}, &cc); err != nil {
		writeKubeError(w, err)
		return
	}
	_, valid, err := a.binding(r.Context(), &cc)
	if err != nil {
		writeKubeError(w, err)
		return
	}
	var op ext.NodeEnrollment
	if err := a.Client.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: name}, &op); err != nil {
		writeKubeError(w, err)
		return
	}
	if !valid || string(cc.UID) != body.ClusterUID || string(op.UID) != body.UID || op.Spec.ClusterRecord.Name != cc.Name || op.Spec.ClusterRecord.Namespace != cc.Namespace || op.Spec.ClusterRecord.UID != string(cc.UID) || tenant.IDOf(&op) != tenant.IDOf(&cc) || op.Spec.TenantID != tenant.IDOf(&cc) || !op.DeletionTimestamp.IsZero() || !cc.DeletionTimestamp.IsZero() {
		writeError(w, 409, "conflict", "operation or cluster identity changed")
		return
	}
	if !op.Spec.CancelRequested && op.Status.Phase != "Succeeded" && op.Status.Phase != "Failed" && op.Status.Phase != "Cancelled" {
		before := op.DeepCopy()
		op.Spec.CancelRequested = true
		if err := a.Client.Patch(r.Context(), &op, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			writeKubeError(w, err)
			return
		}
	}
	slog.InfoContext(r.Context(), "container enrollment cancellation requested", "subject", subject, "operationID", op.Spec.OperationID, "clusterUID", cc.UID)
	writeJSON(w, http.StatusAccepted, summarizeEnrollment(op))
}

// EnrollmentCreator accepts administrator operations without handling SSH keys.
type EnrollmentCreator interface {
	CreateEnrollment(context.Context, provision.EnrollmentRequest) (*ext.NodeEnrollment, error)
}

func (a *API) startNodes(w http.ResponseWriter, r *http.Request, namespace, name, subject string) {
	if a.Enrollments == nil {
		writeError(w, 404, "unsupported", "Notebook enrollment disabled")
		return
	}
	var body struct {
		OperationID string               `json:"operationID"`
		ClusterUID  string               `json:"clusterUID"`
		Release     string               `json:"release"`
		Nodes       []ext.EnrollmentNode `json:"nodes"`
	}
	if !decodeStrict(w, r, &body) {
		return
	}
	for i := range body.Nodes {
		if body.Nodes[i].Port == 0 {
			body.Nodes[i].Port = 22
		}
	}
	if err := provision.ValidateEnrollmentNodes(body.Nodes); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	var cc ext.ContainerCluster
	if err := a.Client.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: name}, &cc); err != nil {
		writeKubeError(w, err)
		return
	}
	_, valid, err := a.binding(r.Context(), &cc)
	if err != nil {
		writeKubeError(w, err)
		return
	}
	if !valid || string(cc.UID) != body.ClusterUID || cc.Spec.Runtime != "Raw" || !cc.DeletionTimestamp.IsZero() {
		writeError(w, 409, "conflict", "cluster binding changed or is unsupported")
		return
	}
	op, err := a.Enrollments.CreateEnrollment(r.Context(), provision.EnrollmentRequest{OperationID: body.OperationID, Namespace: namespace, Cluster: name, ClusterUID: body.ClusterUID, TenantID: tenant.IDOf(&cc), Release: body.Release, Nodes: body.Nodes})
	if err != nil {
		writeKubeError(w, err)
		return
	}
	slog.InfoContext(r.Context(), "container start/join requested", "subject", subject, "operationID", body.OperationID, "clusterUID", cc.UID)
	writeJSON(w, http.StatusAccepted, summarizeEnrollment(*op))
}
