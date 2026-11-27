package clusterupgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrExecutorInterrupted is retryable by the same Job. It denotes a Pod
// being removed or replaced while an upgrade is in progress.
var ErrExecutorInterrupted = errors.New("upgrade executor Pod was interrupted")

type JobRequest struct {
	JobUID, PodName, PodUID, Deployment string
	DeployConfig                        string
	Mode, Cluster, ChildName            string
	AutoRollback                        bool
	HealthTimeout                       time.Duration
}

// RunInCluster executes the mode approved by the member policy.
func RunInCluster(ctx context.Context, request JobRequest) error {
	config, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	config.Timeout = 15 * time.Second
	members, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	reader, err := client.New(config, client.Options{})
	if err != nil {
		return err
	}
	return runImageJob(ctx, members, reader, request)
}

func runImageJob(ctx context.Context, member kubernetes.Interface, reader client.Reader, request JobRequest) error {
	if request.JobUID == "" || request.PodName == "" || request.PodUID == "" || request.Deployment == "" || request.HealthTimeout <= 0 {
		return errors.New("invalid image-only upgrade executor identity or timeout")
	}
	policy, err := ResolveJobPolicy(ctx, reader)
	if err != nil {
		return err
	}
	mode := request.Mode
	if mode == "" {
		mode = ClusterOnlyMode
	}
	if policy == nil || policy.Deployment != request.Deployment || policy.Mode != mode {
		return errors.New("no matching image-only upgrade executor policy")
	}
	managed := mode == ManagedNodeMode
	target, nodeImage, err := parseImagePlan(request.DeployConfig, managed)
	if err != nil {
		return err
	}
	identity := ExecutorIdentity{ChildUID: request.JobUID, ChildName: request.ChildName, PodName: request.PodName, PodUID: request.PodUID}
	pod, err := readExecutor(ctx, member, identity)
	if err != nil {
		return err
	}
	if err := policy.Validate(ctx, reader, executorImage(pod), request.DeployConfig); err != nil {
		return err
	}
	holder := "upgrade-job/" + request.JobUID
	jobUID := string(metav1.GetControllerOf(pod).UID)
	confirmExited := func(ctx context.Context, previousHolder, previous string) (bool, error) {
		return executorExited(ctx, member, previousHolder, previous, holder, jobUID)
	}
	leases := member.CoordinationV1().Leases(Namespace)
	var guard *Guard
	var work context.Context
	if err := wait.PollUntilContextTimeout(ctx, 2*time.Second, time.Duration(leaseDurationSeconds+10)*time.Second, true, func(call context.Context) (bool, error) {
		if _, err := readExecutor(call, member, identity); err != nil {
			return false, err
		}
		// The guard belongs to the Job lifetime, not the polling deadline.
		guard, work, err = Acquire(ctx, leases, holder, leaseExecutor(pod), confirmExited)
		var conflict *LeaseConflict
		if errors.As(err, &conflict) && conflict.Holder == holder {
			return false, nil
		}
		if apierrors.IsConflict(err) {
			return false, nil
		}
		return err == nil, err
	}); err != nil {
		return fmt.Errorf("acquire upgrade executor lease: %w", err)
	}
	check := func(ctx context.Context) error {
		// A force-deleted Pod can still have a running process. Before
		// accepting a write, also require its original live Pod and Job.
		if _, err := readExecutor(ctx, member, identity); err != nil {
			if apierrors.IsNotFound(err) || errors.Is(err, ErrUnverifiedExecutor) {
				return errors.Join(ErrExecutorInterrupted, err)
			}
			return err
		}
		return guard.Check(ctx)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = guard.Release(cleanup)
	}()
	checkpoint := ConfigMapCheckpoint{ConfigMaps: member.CoreV1().ConfigMaps(Namespace), JobUID: request.JobUID, CheckLease: check}
	var nodes NodeAgentImages = clusterOnlyNodeImages{reader: reader}
	if managed {
		if request.Cluster == "" {
			return errors.New("managed node upgrade requires the member cluster identity")
		}
		httpClient, err := brokerTLSClient(policy.BrokerOrigin, policy.BrokerCA)
		if err != nil {
			return err
		}
		adapter, err := NewBrokerNodeImages(policy.BrokerOrigin, request.Cluster, nodeImage, target, identity, readBrokerToken, httpClient)
		if err != nil {
			return err
		}
		saved, exists, err := checkpoint.Load(work)
		if err != nil {
			return err
		}
		if exists {
			// A replacement Pod must prove the original grant still exists
			// before it can perform any further Deployment writes.
			grant, err := adapter.call(work, ReadNodeImage)
			if err != nil {
				return err
			}
			if !matchesNodeSnapshot(grant, saved.NodeAgent) || (grant.Phase == NodeImageRolledBack && !saved.RollingBack) {
				return ErrNodeImageGrant
			}
		}
		nodes = adapter
	} else if err := ValidateClusterOnly(work, reader); err != nil {
		// Recheck topology after taking the lease, even on checkpoint recovery.
		return err
	}
	executor := Executor{
		Deployments: member.AppsV1().Deployments(Namespace), ExpectedDeployment: request.Deployment,
		HealthTimeout: request.HealthTimeout, Checkpoints: checkpoint, CheckLease: check, NodeAgents: nodes,
	}
	return executor.Run(work, target, nodeImage, request.AutoRollback)
}

// A vanished Pod is only replaceable by a live Pod of the same immutable Job
// after lease expiry. Unrelated attempts still need positive terminal evidence.
func executorExited(ctx context.Context, member kubernetes.Interface, previousHolder, previous, holder, jobUID string) (bool, error) {
	parts := strings.Split(previous, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return false, nil
	}
	old, err := member.CoreV1().Pods(Namespace).Get(ctx, parts[0], metav1.GetOptions{})
	sameJob := previousHolder == holder && len(parts) == 3 && parts[2] != "" && parts[2] == jobUID
	if apierrors.IsNotFound(err) || err == nil && string(old.UID) != parts[1] {
		return sameJob, nil
	}
	if err != nil {
		return false, err
	}
	child, ok := strings.CutPrefix(previousHolder, "upgrade-job/")
	owner := metav1.GetControllerOf(old)
	if !ok || child == "" || owner == nil || (owner.Name != JobName(child) && owner.Name != LockedStockJobName(child)) || owner.Kind != "Job" ||
		owner.APIVersion != batchv1.SchemeGroupVersion.String() || (len(parts) == 3 && string(owner.UID) != parts[2]) {
		return false, nil
	}
	return old.Status.Phase == corev1.PodSucceeded || old.Status.Phase == corev1.PodFailed, nil
}

// PreviousUpgradeExecutorExited allows a release to take over a terminal
// executor after expiry. Missing or replaced Pods are not positive evidence.
func PreviousUpgradeExecutorExited(ctx context.Context, member kubernetes.Interface, holder, executor string) (bool, error) {
	return executorExited(ctx, member, holder, executor, "", "")
}

const BrokerTokenMount = "/var/run/rlark-upgrade/broker"

func readBrokerToken() (string, error) {
	data, err := os.ReadFile(BrokerTokenMount + "/token")
	return strings.TrimSpace(string(data)), err
}

type clusterOnlyNodeImages struct{ reader client.Reader }

func (n clusterOnlyNodeImages) Read(ctx context.Context) (NodeAgentTarget, error) {
	return NodeAgentTarget{}, ValidateClusterOnly(ctx, n.reader)
}
func (clusterOnlyNodeImages) CompareAndSet(context.Context, NodeAgentTarget, string, string, bool) error {
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
	case errors.Is(err, ErrBatchAuthority):
		diagnostic.Kind = "managed-authority"
	case errors.Is(err, ErrManagedBaseline):
		diagnostic.Kind = "managed-baseline"
	}
	data, marshalErr := json.Marshal(diagnostic)
	if marshalErr == nil {
		_ = os.WriteFile("/dev/termination-log", data, 0o600)
	}
}
