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
	Context           context.Context
	Check             func(context.Context) error
	Release           func()
	state             core.ConfigMapInterface
	childUID, image   string
	jobUID, podUID    string
	mutationRecorded  bool
	beforeMutation    func(context.Context) error
	previousJobExited func(context.Context, stockCheckpoint) (bool, error)
	stop              func()
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
	var policy *JobPolicy
	if err := retryMemberAPI(ctx, func(call context.Context) error {
		var err error
		policy, err = ResolveJobPolicy(call, reader)
		return err
	}); err != nil {
		return nil, stockInterrupted(ctx, err)
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
	holder := "upgrade-job/" + identity.ChildUID
	var job *batchv1.Job
	var guard *Guard
	var work context.Context
	var lastErr error
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, time.Duration(executorHandoffSeconds)*time.Second, true, func(ctxPoll context.Context) (bool, error) {
		verified, observed, err := readPod(ctxPoll)
		if err != nil {
			lastErr = err
			if temporaryAPIError(err) {
				return false, nil
			}
			return false, err
		}
		job = observed
		guard, work, err = Acquire(ctx, member.CoordinationV1().Leases(Namespace), holder, leaseExecutor(verified),
			func(call context.Context, previousHolder, executor string) (bool, error) {
				return executorExited(call, member, previousHolder, executor, holder, string(observed.UID))
			})
		lastErr = err
		var conflict *LeaseConflict
		if (errors.As(err, &conflict) && conflict.Holder == holder) || apierrors.IsConflict(err) || temporaryAPIError(err) {
			return false, nil
		}
		return err == nil, err
	})
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) && lastErr != nil {
			err = lastErr
		}
		return nil, stockInterrupted(ctx, err)
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
	return &LockedStock{Context: work, Check: check, Release: release, state: member.CoreV1().ConfigMaps(Namespace), childUID: identity.ChildUID, image: policy.ExecutorImage, jobUID: string(job.UID), podUID: identity.PodUID,
		mutationRecorded: guard.stockMutationRecorded,
		beforeMutation:   guard.recordStockMutation,
		previousJobExited: func(call context.Context, saved stockCheckpoint) (bool, error) {
			old, err := member.BatchV1().Jobs(Namespace).Get(call, LockedStockJobName(saved.ChildUID), metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			if err != nil {
				return false, err
			}
			if string(old.UID) != saved.JobUID {
				return true, nil
			}
			return old.Labels[ChildUIDLabel] == saved.ChildUID && upgradeJobTerminal(old), nil
		},
		stop: guard.stopRenewing}, nil
}
