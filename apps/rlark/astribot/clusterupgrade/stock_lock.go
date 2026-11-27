package clusterupgrade

import (
	"context"
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
}

func (s *LockedStock) ReadState(ctx context.Context) (*corev1.ConfigMap, error) {
	return s.state.Get(ctx, StockStateName, metav1.GetOptions{})
}

func (s *LockedStock) SaveState(ctx context.Context, baseline *corev1.ConfigMap, applied string) error {
	record := baseline.DeepCopy()
	if record.Data == nil {
		record.Data = map[string]string{}
	}
	record.Data["image"] = s.image
	record.Data["deploy-config"] = applied
	record.Data[StockAttemptKey] = s.childUID
	if err := s.Check(ctx); err != nil {
		return err
	}
	_, err := s.state.Update(ctx, record, metav1.UpdateOptions{})
	if err == nil {
		return nil
	}
	current, readErr := s.ReadState(ctx)
	if readErr == nil && current.Data[StockAttemptKey] == s.childUID &&
		current.Data["image"] == s.image && current.Data["deploy-config"] == applied {
		return nil
	}
	return errors.Join(err, readErr)
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
	readPod := func(ctx context.Context) (*corev1.Pod, error) {
		return readOwnedExecutor(ctx, member, identity, LockedStockJobName(identity.ChildUID), StockJobServiceAccount)
	}
	pod, err := readPod(ctx)
	if err != nil {
		return nil, err
	}
	if executorImage(pod) != policy.ExecutorImage {
		return nil, ErrUnverifiedExecutor
	}
	guard, work, err := Acquire(ctx, member.CoordinationV1().Leases(Namespace), "upgrade-job/"+identity.ChildUID, leaseExecutor(pod),
		func(ctx context.Context, holder, executor string) (bool, error) {
			return PreviousUpgradeExecutorExited(ctx, member, holder, executor)
		})
	if err != nil {
		return nil, err
	}
	check := func(ctx context.Context) error {
		if _, err := readPod(ctx); err != nil {
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
	return &LockedStock{Context: work, Check: check, Release: release, state: member.CoreV1().ConfigMaps(Namespace), childUID: identity.ChildUID, image: policy.ExecutorImage}, nil
}
