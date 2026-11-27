package clusterupgrade

import (
	"context"
	"errors"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const StockStateName = "rlark-state"
const StockAttemptKey = "upgrade-attempt-uid"

type LockedStock struct {
	Context         context.Context
	Check           func(context.Context) error
	Release         func()
	state           core.ConfigMapInterface
	childUID, image string
	jobUID, podUID  string
	replacement     bool
	stop            func()
}

func (s *LockedStock) ReadState(ctx context.Context) (*corev1.ConfigMap, error) {
	return s.state.Get(ctx, StockStateName, metav1.GetOptions{})
}

// HoldStockInCluster acquires the same member Lease as rlarkctl release.
// The old upstream image cannot opt itself in: the member policy pins the
// actual executor digest, and the stock Job and Pod identities are verified.
func HoldStockInCluster(ctx context.Context, identity ExecutorIdentity) (*LockedStock, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	config.Timeout = 15 * time.Second
	member, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	reader, err := client.New(config, client.Options{})
	if err != nil {
		return nil, err
	}
	return holdStock(ctx, member, reader, identity)
}

func holdStock(ctx context.Context, member kubernetes.Interface, reader client.Reader, identity ExecutorIdentity) (*LockedStock, error) {
	policy, err := ResolveJobPolicy(ctx, reader)
	if err != nil {
		return nil, err
	}
	if policy == nil || policy.Mode != DriverStock {
		return nil, errors.New("member has no enabled locked-stock upgrade policy")
	}
	readPod := func(ctx context.Context) (*corev1.Pod, *batchv1.Job, error) {
		pod, job, err := readOwnedExecutorJob(ctx, member, identity, LockedStockJobName(identity.ChildUID), StockJobServiceAccount)
		if err == nil && executorImage(pod) != policy.ExecutorImage {
			return nil, nil, ErrUnverifiedExecutor
		}
		return pod, job, err
	}
	_, job, err := readPod(ctx)
	if err != nil {
		return nil, err
	}
	holder, jobUID := "upgrade-job/"+identity.ChildUID, string(job.UID)
	var guard *Guard
	var work context.Context
	err = wait.PollUntilContextTimeout(ctx, 2*time.Second, time.Duration(executorHandoffSeconds)*time.Second, true, func(ctxPoll context.Context) (bool, error) {
		verified, observed, err := readPod(ctxPoll)
		if err != nil {
			if temporaryAPIError(err) {
				return false, nil
			}
			return false, err
		}
		job = observed
		guard, work, err = Acquire(ctx, member.CoordinationV1().Leases(Namespace), holder, leaseExecutor(verified),
			func(call context.Context, previousHolder, executor string) (bool, error) {
				return executorExited(call, member, previousHolder, executor, holder, jobUID)
			})
		var conflict *LeaseConflict
		if (errors.As(err, &conflict) && conflict.Holder == holder) || apierrors.IsConflict(err) || temporaryAPIError(err) {
			return false, nil
		}
		return err == nil, err
	})
	if err != nil {
		return nil, err
	}
	check := func(ctx context.Context) error {
		if _, _, err := readPod(ctx); err != nil {
			if apierrors.IsNotFound(err) || errors.Is(err, ErrUnverifiedExecutor) {
				return errors.Join(ErrExecutorInterrupted, err)
			}
			return err
		}
		return guard.Check(ctx)
	}
	release := func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := guard.Release(cleanup); err != nil {
			log.FromContext(ctx).Error(err, "release stock member upgrade lease")
		}
	}
	return &LockedStock{Context: work, Check: check, Release: release, state: member.CoreV1().ConfigMaps(Namespace), childUID: identity.ChildUID, image: policy.ExecutorImage, jobUID: jobUID, podUID: identity.PodUID,
		replacement: guard.handoff || job.Status.Failed > 0 || job.Status.Succeeded > 0 ||
			(job.Status.UncountedTerminatedPods != nil && (len(job.Status.UncountedTerminatedPods.Failed) > 0 || len(job.Status.UncountedTerminatedPods.Succeeded) > 0)),
		stop: func() { guard.cancel(context.Canceled); <-guard.done }}, nil
}
