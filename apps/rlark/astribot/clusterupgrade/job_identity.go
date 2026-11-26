package clusterupgrade

import (
	"context"
	"errors"
	"slices"

	authv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	BrokerAudience = "rlark-node-agent-upgrade"
	ChildUIDLabel  = "rlark.io/cluster-upgrade-job-uid"
)

var ErrUnverifiedExecutor = errors.New("upgrade executor identity could not be verified")

// JobName is unique for one management ClusterUpgradeJob UID.
func JobName(childUID string) string { return "upgrade-attempt-" + childUID }

type ExecutorIdentity struct {
	ChildUID, ChildName, PodName, PodUID string
}

func readExecutor(ctx context.Context, member kubernetes.Interface, identity ExecutorIdentity) (*corev1.Pod, error) {
	if identity.ChildUID == "" || identity.PodName == "" || identity.PodUID == "" {
		return nil, ErrUnverifiedExecutor
	}
	pod, err := member.CoreV1().Pods(Namespace).Get(ctx, identity.PodName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	owner := metav1.GetControllerOf(pod)
	if pod.DeletionTimestamp != nil || string(pod.UID) != identity.PodUID || pod.Spec.ServiceAccountName != JobServiceAccount ||
		owner == nil || owner.Kind != "Job" || owner.APIVersion != batchv1.SchemeGroupVersion.String() || owner.Name != JobName(identity.ChildUID) {
		return nil, ErrUnverifiedExecutor
	}
	job, err := member.BatchV1().Jobs(Namespace).Get(ctx, owner.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if job.DeletionTimestamp != nil || job.UID == "" || job.UID != owner.UID || job.Labels[ChildUIDLabel] != identity.ChildUID {
		return nil, ErrUnverifiedExecutor
	}
	return pod, nil
}

func executorImage(pod *corev1.Pod) string {
	for _, container := range pod.Spec.Containers {
		if container.Name == "upgrade" {
			return container.Image
		}
	}
	return ""
}

// VerifyExecutor authenticates the projected Pod-bound token on the intended
// member API, then rechecks the Pod's owner, the approved executor digest and
// the Lease. A legacy unbound ServiceAccount token cannot pass this check.
func VerifyExecutor(ctx context.Context, member kubernetes.Interface, token, approvedImage string, identity ExecutorIdentity) error {
	if token == "" || identity.PodUID == "" || identity.PodName == "" || identity.ChildUID == "" {
		return ErrUnverifiedExecutor
	}
	if _, err := pinnedImage(approvedImage); err != nil {
		return ErrUnverifiedExecutor
	}
	review, err := member.AuthenticationV1().TokenReviews().Create(ctx, &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{Token: token, Audiences: []string{BrokerAudience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	status := review.Status
	podNames := status.User.Extra["authentication.kubernetes.io/pod-name"]
	podUIDs := status.User.Extra["authentication.kubernetes.io/pod-uid"]
	if !status.Authenticated || status.Error != "" || !slices.Contains(status.Audiences, BrokerAudience) ||
		status.User.Username != "system:serviceaccount:"+Namespace+":"+JobServiceAccount ||
		len(podNames) != 1 || podNames[0] != identity.PodName || len(podUIDs) != 1 || podUIDs[0] != identity.PodUID {
		return ErrUnverifiedExecutor
	}
	pod, err := readExecutor(ctx, member, identity)
	if err != nil {
		return err
	}
	// A container can start before the kubelet publishes PodRunning. The
	// bound token and held lease still identify the executing Pod.
	if (pod.Status.Phase != corev1.PodPending && pod.Status.Phase != corev1.PodRunning) || executorImage(pod) != approvedImage {
		return ErrUnverifiedExecutor
	}
	guard := Guard{
		leases: member.CoordinationV1().Leases(Namespace),
		holder: "upgrade-job/" + identity.ChildUID, executor: identity.PodName + "/" + identity.PodUID,
	}
	return guard.Check(ctx)
}
