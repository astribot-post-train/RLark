package clusterupgrade

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type JobRequest struct {
	JobUID, PodName, PodUID, Deployment string
	DeployConfig                        string
	AutoRollback                        bool
	HealthTimeout                       time.Duration
}

// RunInCluster executes the explicit cluster-only mode. An installation with
// managed node Agents needs a separate, authorized management-plane adapter.
func RunInCluster(ctx context.Context, request JobRequest) error {
	config, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	members, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	reader, err := client.New(config, client.Options{})
	if err != nil {
		return err
	}
	return runClusterOnly(ctx, members, reader, request)
}

func runClusterOnly(ctx context.Context, member kubernetes.Interface, reader client.Reader, request JobRequest) error {
	if request.JobUID == "" || request.PodName == "" || request.PodUID == "" || request.Deployment == "" || request.HealthTimeout <= 0 {
		return errors.New("invalid image-only upgrade executor identity or timeout")
	}
	target, err := ParseImages(request.DeployConfig)
	if err != nil {
		return err
	}
	policy, err := ResolveJobPolicy(ctx, reader)
	if err != nil {
		return err
	}
	if policy == nil || policy.Deployment != request.Deployment {
		return errors.New("no matching image-only upgrade executor policy")
	}
	pods := member.CoreV1().Pods(Namespace)
	pod, err := pods.Get(ctx, request.PodName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	owner := metav1.GetControllerOf(pod)
	if pod.DeletionTimestamp != nil || string(pod.UID) != request.PodUID || pod.Spec.ServiceAccountName != JobServiceAccount ||
		owner == nil || owner.Kind != "Job" || owner.APIVersion != batchv1.SchemeGroupVersion.String() {
		return errors.New("invalid image-only upgrade Pod identity")
	}
	job, err := member.BatchV1().Jobs(Namespace).Get(ctx, owner.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if job.DeletionTimestamp != nil || job.UID != owner.UID || job.Name != "upgrade-attempt-"+request.JobUID || job.Labels["rlark.io/cluster-upgrade-job-uid"] != request.JobUID {
		return errors.New("invalid image-only upgrade Job identity")
	}
	image := ""
	for _, container := range pod.Spec.Containers {
		if container.Name == "upgrade" {
			image = container.Image
		}
	}
	if err := policy.Validate(ctx, reader, image, request.DeployConfig); err != nil {
		return err
	}
	confirmExited := func(ctx context.Context, _ string, previous string) (bool, error) {
		name, uid, ok := strings.Cut(previous, "/")
		if !ok {
			return false, nil
		}
		old, err := pods.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			// A missing Pod cannot prove that its old node stopped running it.
			return false, nil
		}
		return string(old.UID) == uid && (old.Status.Phase == corev1.PodSucceeded || old.Status.Phase == corev1.PodFailed), nil
	}
	guard, work, err := Acquire(ctx, member.CoordinationV1().Leases(Namespace), "upgrade-job/"+request.JobUID, request.PodName+"/"+request.PodUID, confirmExited)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = guard.Release(cleanup)
	}()
	checkpoint := ConfigMapCheckpoint{ConfigMaps: member.CoreV1().ConfigMaps(Namespace), JobUID: request.JobUID, CheckLease: guard.Check}
	executor := Executor{
		Deployments: member.AppsV1().Deployments(Namespace), ExpectedDeployment: request.Deployment,
		HealthTimeout: request.HealthTimeout, Checkpoints: checkpoint, CheckLease: guard.Check,
		NodeAgents: clusterOnlyNodeImages{reader: reader},
	}
	// Recheck topology after taking the lease, even on checkpoint recovery.
	if err := ValidateClusterOnly(work, reader); err != nil {
		return err
	}
	return executor.Run(work, target, request.AutoRollback)
}

type clusterOnlyNodeImages struct{ reader client.Reader }

func (n clusterOnlyNodeImages) Read(ctx context.Context) (NodeAgentTarget, error) {
	return NodeAgentTarget{}, ValidateClusterOnly(ctx, n.reader)
}
func (clusterOnlyNodeImages) CompareAndSet(context.Context, NodeAgentTarget, string, string) error {
	return errors.New("node Agents are outside cluster-only upgrade mode")
}
func (clusterOnlyNodeImages) WaitReady(context.Context, NodeAgentTarget, string) error {
	return errors.New("node Agents are outside cluster-only upgrade mode")
}

func WriteJobDiagnostic(err error) {
	diagnostic := Diagnostic{Kind: "execution-failed"}
	var conflict *LeaseConflict
	var field *FieldError
	switch {
	case errors.As(err, &conflict):
		diagnostic = Diagnostic{Kind: "lock-conflict", Holder: conflict.Holder}
	case errors.As(err, &field):
		diagnostic = Diagnostic{Kind: "unsupported-field", Field: field.Field}
	case errors.Is(err, ErrRollbackFailed) && errors.Is(err, ErrTargetChanged):
		diagnostic.Kind = "rollback-conflict"
	case errors.Is(err, ErrRolledBack):
		diagnostic.Kind = "rollout-failed"
	}
	data, marshalErr := json.Marshal(diagnostic)
	if marshalErr == nil {
		_ = os.WriteFile("/dev/termination-log", data, 0o600)
	}
}
