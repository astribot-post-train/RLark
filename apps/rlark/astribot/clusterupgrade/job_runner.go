package clusterupgrade

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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
	pods := member.CoreV1().Pods(Namespace)
	identity := ExecutorIdentity{ChildUID: request.JobUID, ChildName: request.ChildName, PodName: request.PodName, PodUID: request.PodUID}
	pod, err := readExecutor(ctx, member, identity)
	if err != nil {
		return err
	}
	if err := policy.Validate(ctx, reader, executorImage(pod), request.DeployConfig); err != nil {
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
		HealthTimeout: request.HealthTimeout, Checkpoints: checkpoint, CheckLease: guard.Check, NodeAgents: nodes,
	}
	return executor.Run(work, target, nodeImage, request.AutoRollback)
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
