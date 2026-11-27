package workflow

import (
	"context"
	"encoding/json"
	"fmt"

	rlark "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/stationidentity"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

const nodeJoinCleanupAnnotation = "station.rlark.io/admission-cleanup"
const retiredAdmissionExpiry = "1970-01-01T00:00:00Z"

// The Admission update fences redemption before we copy ownership evidence
// onto the join. The receipt survives deletion of the Admission.
type joinAdmissionCleanup struct {
	JoinUID                  types.UID
	Namespace, Cluster, Node string
	AdmissionUID             types.UID
	AdmissionVersion         string
	Hub                      bool
	PublicKey, TunnelAddress string
}

func cleanupAdmissionScope(j *rlark.NodeJoin, namespace string) joinAdmissionCleanup {
	return joinAdmissionCleanup{
		JoinUID: j.UID, Namespace: namespace, Cluster: j.Spec.ClusterID, Node: j.Spec.NodeName,
		Hub: j.Status.Facts.DeclaredHub, PublicKey: j.Status.Facts.PublicKey, TunnelAddress: j.Status.Facts.TunnelAddress,
	}
}

func admissionCleanupConflict(j *rlark.NodeJoin, message string) error {
	return apierrors.NewConflict(nodeAdmissionGVR.GroupResource(), j.Spec.NodeName, fmt.Errorf("%s", message))
}

func readJoinAdmissionCleanup(j *rlark.NodeJoin, namespace string) (joinAdmissionCleanup, error) {
	var receipt joinAdmissionCleanup
	raw := j.Annotations[nodeJoinCleanupAnnotation]
	if raw == "" {
		return receipt, nil
	}
	if err := json.Unmarshal([]byte(raw), &receipt); err != nil || receipt.AdmissionUID == "" || receipt.AdmissionVersion == "" {
		return receipt, admissionCleanupConflict(j, "the stored Admission cleanup receipt is invalid")
	}
	expect := cleanupAdmissionScope(j, namespace)
	expect.AdmissionUID, expect.AdmissionVersion = receipt.AdmissionUID, receipt.AdmissionVersion
	if expect != receipt || (receipt.Hub && (receipt.PublicKey == "" || receipt.TunnelAddress == "")) {
		return receipt, admissionCleanupConflict(j, "the stored Admission cleanup receipt belongs to a different join or declaration")
	}
	return receipt, nil
}

func (g *Service) storeJoinAdmissionCleanup(ctx context.Context, j *rlark.NodeJoin, receipt joinAdmissionCleanup) error {
	client := g.kubeClient.RlinfV1alpha1().NodeJoins()
	current, err := client.Get(ctx, j.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if current.UID != j.UID || current.ResourceVersion != j.ResourceVersion || current.DeletionTimestamp != nil ||
		current.Annotations[nodeJoinNodeUID] != "" || current.Status.Phase == rlark.ClusterProvisionRunning {
		return admissionCleanupConflict(j, "the join changed before its Admission cleanup receipt could be recorded")
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	metav1.SetMetaDataAnnotation(&current.ObjectMeta, nodeJoinCleanupAnnotation, string(encoded))
	written, err := client.Update(ctx, current, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	j.ResourceVersion = written.ResourceVersion
	j.Annotations = written.Annotations
	return nil
}

// retireJoinAdmission verifies and expires the original code, records the exact
// retired Admission version, then waits for disappearance before clearing its Hub.
func (g *Service) retireJoinAdmission(ctx context.Context, j *rlark.NodeJoin, namespace string, admission *unstructured.Unstructured, receipt joinAdmissionCleanup) error {
	scope := cleanupAdmissionScope(j, namespace)
	if scope.Hub && (scope.PublicKey == "" || scope.TunnelAddress == "") {
		return admissionCleanupConflict(j, "the Hub declaration has no verified key or address for cleanup")
	}
	resource := g.stationClient.Resource(nodeAdmissionGVR).Namespace(namespace)
	uid := admission.GetUID()
	if uid == "" || admission.GetResourceVersion() == "" ||
		(receipt.AdmissionUID != "" && receipt.AdmissionUID != uid) {
		return admissionCleanupConflict(j, "the Admission identity does not match the original cleanup")
	}
	marker := admission.GetAnnotations()[stationidentity.AdmissionCleanupAnnotation]
	if (receipt.AdmissionUID != "" || marker != "") && marker != string(j.UID) {
		return admissionCleanupConflict(j, "the Admission retirement owner changed")
	}
	if admission.GetDeletionTimestamp() != nil {
		return admissionCleanupConflict(j, "the original Admission is still being deleted; retry cleanup")
	}
	expiry, _, err := unstructured.NestedString(admission.Object, "spec", "expiresAt")
	if err != nil {
		return err
	}
	if marker == "" {
		annotations := admission.GetAnnotations()
		annotations[stationidentity.AdmissionCleanupAnnotation] = string(j.UID)
		admission.SetAnnotations(annotations)
		if err := unstructured.SetNestedField(admission.Object, retiredAdmissionExpiry, "spec", "expiresAt"); err != nil {
			return err
		}
		admission, err = resource.Update(ctx, admission, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("retire the unused SSH admission: %w", err)
		}
	} else if expiry != retiredAdmissionExpiry {
		return admissionCleanupConflict(j, "the retired Admission expiry changed")
	}
	version := admission.GetResourceVersion()
	if admission.GetUID() != uid || version == "" {
		return admissionCleanupConflict(j, "the retired Admission identity could not be verified")
	}
	if receipt.AdmissionVersion != version || receipt.AdmissionUID == "" {
		receipt = scope
		receipt.AdmissionUID, receipt.AdmissionVersion = uid, version
		if err := g.storeJoinAdmissionCleanup(ctx, j, receipt); err != nil {
			return fmt.Errorf("record the original SSH admission cleanup: %w", err)
		}
	}
	if err := resource.Delete(ctx, admission.GetName(), metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version},
	}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete the retired SSH admission: %w", err)
	}
	if _, err := resource.Get(ctx, admission.GetName(), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		if err != nil {
			return err
		}
		return admissionCleanupConflict(j, "the original Admission has not cleared or a replacement exists; retry cleanup")
	}
	return nil
}
