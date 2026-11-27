package stationidentity

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// AdmissionService issues codes for cluster namespaces already resolved by the caller.
type AdmissionService struct {
	SiteNetwork func(context.Context) (*unstructured.Unstructured, error)
	Admissions  func(string) dynamic.ResourceInterface
	NodeClasses NodeClassReader
	Bootstrap   AdmissionBootstrap
}

// AdmissionError carries the admission status and message for HTTP and internal callers.
type AdmissionError struct {
	Status int
	err    error
}

func (e *AdmissionError) Error() string { return e.err.Error() }
func (e *AdmissionError) Unwrap() error { return e.err }

func refuseAdmission(status int, message string) *AdmissionError {
	return &AdmissionError{Status: status, err: errors.New(message)}
}

// Issue writes the admission of a machine in the cluster's namespace and
// makes its code and its command: what the code tab and the SSH tab both do,
// and what any part of the Gateway that joins a machine calls.
func (svc AdmissionService) Issue(ctx context.Context, actor, namespace string, request *IssueAdmissionRequest, marks map[string]string) (IssuedAdmission, int, *AdmissionError) {
	ttl, err := JoinCodeTTL(request.TTLSeconds)
	if err == nil {
		err = ValidateIssueAdmission(request)
	}
	if err != nil {
		return IssuedAdmission{}, 0, &AdmissionError{Status: http.StatusBadRequest, err: err}
	}
	network, caDigest, code, codeHash, refused := svc.newJoinCodeFor(ctx)
	if refused != nil {
		return IssuedAdmission{}, 0, refused
	}
	if request.Site != "" {
		declared := false
		sites, _, _ := unstructured.NestedSlice(network.Object, "spec", "sites")
		for _, raw := range sites {
			entry, _ := raw.(map[string]interface{})
			declared = declared || entry["name"] == request.Site
		}
		if !declared {
			return IssuedAdmission{}, 0, refuseAdmission(http.StatusBadRequest, "the SiteNetwork has no site "+request.Site)
		}
	}
	now := time.Now().UTC()
	spec := map[string]interface{}{
		"clusterID": request.ClusterID, "codeHash": codeHash,
		"expiresAt": now.Add(ttl).Format(time.RFC3339), "issuedBy": actor, "issuedAt": now.Format(time.RFC3339),
	}
	if request.Site != "" {
		spec["site"] = request.Site
	}
	if request.Address != "" {
		spec["address"] = request.Address
	}
	if request.Uplink != "" {
		spec["uplink"] = map[string]interface{}{"gateway": request.Uplink}
	}
	if request.NodeClass != "" {
		spec["nodeClass"] = request.NodeClass
	}
	if len(request.Taints) > 0 {
		taints := make([]interface{}, len(request.Taints))
		for i, taint := range request.Taints {
			taints[i] = taint
		}
		spec["taints"] = taints
	}
	if len(request.Labels) > 0 {
		labels := map[string]interface{}{}
		for key, value := range request.Labels {
			labels[key] = value
		}
		spec["labels"] = labels
	}
	if len(request.Machine) > 0 {
		spec["machine"] = request.Machine
	}
	resource := svc.Admissions(namespace)
	existing, err := resource.Get(ctx, request.NodeName, metav1.GetOptions{})
	var written *unstructured.Unstructured
	status := http.StatusOK
	switch {
	case apierrors.IsNotFound(err):
		status = http.StatusCreated
		admission := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "station.rlark.io/v1alpha1", "kind": "NodeAdmission",
			"metadata": map[string]interface{}{"name": request.NodeName, "namespace": namespace}, "spec": spec,
		}}
		admission.SetAnnotations(marks)
		written, err = resource.Create(ctx, admission, metav1.CreateOptions{})
	case err == nil:
		if existing.GetAnnotations()[AdmissionCleanupAnnotation] != "" || existing.GetDeletionTimestamp() != nil {
			return IssuedAdmission{}, 0, refuseAdmission(http.StatusConflict, "the admission is being retired")
		}
		existing.Object["spec"] = spec
		dropJoinMarks(existing, marks)
		written, err = resource.Update(ctx, existing, metav1.UpdateOptions{})
	}
	if refused := nodeAdmissionWriteError(ctx, err); refused != nil {
		return IssuedAdmission{}, 0, refused
	}
	issued, refused := svc.issuedNodeAdmission(ctx, network, written, code, caDigest, "issue")
	return issued, status, refused
}

// Reissue replaces the code on a fetched admission, preserving its declared machine.
func (svc AdmissionService) Reissue(ctx context.Context, actor string, admission *unstructured.Unstructured, resource dynamic.ResourceInterface, ttl time.Duration) (IssuedAdmission, *AdmissionError) {
	if admission.GetAnnotations()[AdmissionCleanupAnnotation] != "" || admission.GetDeletionTimestamp() != nil {
		return IssuedAdmission{}, refuseAdmission(http.StatusConflict, "the admission is being retired")
	}
	network, caDigest, code, codeHash, refused := svc.newJoinCodeFor(ctx)
	if refused != nil {
		return IssuedAdmission{}, refused
	}
	now := time.Now().UTC()
	spec, _, _ := unstructured.NestedMap(admission.Object, "spec")
	for _, field := range []string{"publicKey", "redeemedAt"} {
		delete(spec, field)
	}
	spec["codeHash"], spec["expiresAt"] = codeHash, now.Add(ttl).Format(time.RFC3339)
	spec["issuedBy"], spec["issuedAt"] = actor, now.Format(time.RFC3339)
	admission.Object["spec"] = spec
	dropJoinMarks(admission, nil)
	written, err := resource.Update(ctx, admission, metav1.UpdateOptions{})
	if refused := nodeAdmissionWriteError(ctx, err); refused != nil {
		return IssuedAdmission{}, refused
	}
	return svc.issuedNodeAdmission(ctx, network, written, code, caDigest, "reissue")
}

// Revoke expires a pending code while leaving a previously redeemed admission intact.
func (svc AdmissionService) Revoke(ctx context.Context, actor string, admission *unstructured.Unstructured, resource dynamic.ResourceInterface) (Admission, *AdmissionError) {
	now := time.Now().UTC()
	switch view := AdmissionView(admission, now); view.State {
	case "Pending":
	case "Expired":
		return view, nil
	default:
		return Admission{}, refuseAdmission(http.StatusConflict, "a machine has redeemed the code; remove the node instead (a removal by name also clears a join that never made a node)")
	}
	_ = unstructured.SetNestedField(admission.Object, now.Format(time.RFC3339), "spec", "expiresAt")
	written, err := resource.Update(ctx, admission, metav1.UpdateOptions{})
	if refused := nodeAdmissionWriteError(ctx, err); refused != nil {
		return Admission{}, refused
	}
	view := AdmissionView(written, now)
	log.FromContext(ctx).WithName("audit").Info("node admission", "action", "revoke", "actor", actor,
		"cluster", view.ClusterID, "node", view.NodeName)
	return view, nil
}

// newJoinCodeFor reads what a machine's command is made of (the SiteNetwork, the digest of Station API's
// bootstrap CA) and makes a code: nothing is admitted unless the command can be made.
func (svc AdmissionService) newJoinCodeFor(ctx context.Context) (network *unstructured.Unstructured, caDigest, code, codeHash string, refused *AdmissionError) {
	network, err := svc.SiteNetwork(ctx)
	if err != nil {
		return nil, "", "", "", refuseAdmission(http.StatusBadGateway, "SiteNetwork request failed")
	}
	if caDigest, err = svc.Bootstrap.Digest(ctx, "/v1/bootstrap/ca"); err != nil {
		log.FromContext(ctx).Error(err, "read Station API's bootstrap CA")
		return nil, "", "", "", refuseAdmission(http.StatusBadGateway, "Station API does not serve its bootstrap CA")
	}
	if code, codeHash, err = NewJoinCode(); err != nil {
		return nil, "", "", "", refuseAdmission(http.StatusInternalServerError, "cannot make a code")
	}
	return network, caDigest, code, codeHash, nil
}

// SSHJoinUIDAnnotation associates the code or removal with an SSH join attempt.
const SSHJoinUIDAnnotation = "station.rlark.io/ssh-join-uid"

// AdmissionCleanupAnnotation binds a retired, unused admission to its SSH join.
const AdmissionCleanupAnnotation = "station.rlark.io/ssh-admission-cleanup"

// dropJoinMarks drops the marks of how the admission's machine was joined (a new code drops them) and sets marks.
func dropJoinMarks(admission *unstructured.Unstructured, marks map[string]string) {
	annotations := admission.GetAnnotations()
	for _, key := range []string{AdmissionSSHLoginAnnotation, AdmissionSSHHostKeyAnnotation, AdmissionInstallFailureAnnotation, JoinTokenIssuedAnnotation, SelfHealedAnnotation, SSHJoinUIDAnnotation} {
		delete(annotations, key)
	}
	if len(marks) > 0 && annotations == nil {
		annotations = map[string]string{}
	}
	maps.Copy(annotations, marks)
	admission.SetAnnotations(annotations)
}

func nodeAdmissionWriteError(ctx context.Context, err error) *AdmissionError {
	switch {
	case err == nil:
		return nil
	case apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err):
		return refuseAdmission(http.StatusConflict, "the admission changed meanwhile; try again")
	case apierrors.IsInvalid(err):
		return refuseAdmission(http.StatusBadRequest, "the management plane rejected the admission")
	default:
		log.FromContext(ctx).Error(err, "write NodeAdmission")
		return refuseAdmission(http.StatusBadGateway, "NodeAdmission request failed")
	}
}

// issuedNodeAdmission is the one answer that carries the code and its command.
func (svc AdmissionService) issuedNodeAdmission(ctx context.Context, network, written *unstructured.Unstructured, code, caDigest, action string) (IssuedAdmission, *AdmissionError) {
	view := AdmissionView(written, time.Now())
	command, err := JoinCommand(ctx, network, view.ClusterID, view.NodeName, view.Site, view.NodeClass, code, caDigest, svc.Bootstrap, svc.NodeClasses)
	if err != nil {
		// The admission is written and its code is lost with this answer:
		// a new code replaces it.
		log.FromContext(ctx).Error(err, "make the join command", "cluster", view.ClusterID, "node", view.NodeName)
		return IssuedAdmission{}, refuseAdmission(http.StatusBadGateway, "the admission is written but its command cannot be made; give out a new code")
	}
	log.FromContext(ctx).WithName("audit").Info("node admission", "action", action, "actor", view.IssuedBy,
		"cluster", view.ClusterID, "node", view.NodeName, "expiresAt", view.ExpiresAt)
	return IssuedAdmission{Admission: view, Code: code, Command: command}, nil
}
