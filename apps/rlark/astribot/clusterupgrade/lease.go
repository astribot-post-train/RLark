package clusterupgrade

import (
	"context"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordination "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/utils/ptr"
)

const (
	LeaseName            = "rlark-agent-upgrade"
	Namespace            = "rlark-system"
	leaseDurationSeconds = 300
	renewPeriod          = time.Minute
)

const executorAnnotation = "rlark.io/upgrade-executor"

// LeaseConflict describes who must release the member cluster before it can be
// changed. Expiry alone cannot prove a partitioned writer has exited.
type LeaseConflict struct {
	Holder string
}

func (e *LeaseConflict) Error() string {
	return fmt.Sprintf("member upgrade lease held by %q", e.Holder)
}

// Guard owns a provisioned member-cluster Lease for exactly one executor.
type Guard struct {
	leases   coordination.LeaseInterface
	holder   string
	executor string
	cancel   context.CancelCauseFunc
	done     chan struct{}
	stop     sync.Once
}

// Acquire claims the provisioned Lease with an optimistic concurrency check.
// confirmExited must independently prove the previous executor terminated
// before an expired lease can be taken over. A nil callback refuses takeover.
func Acquire(ctx context.Context, leases coordination.LeaseInterface, holder, executor string, confirmExited func(context.Context, string, string) (bool, error)) (*Guard, context.Context, error) {
	if holder == "" || executor == "" {
		return nil, ctx, fmt.Errorf("lease holder and executor identity are required")
	}
	current, err := leases.Get(ctx, LeaseName, metav1.GetOptions{})
	if err != nil {
		return nil, ctx, fmt.Errorf("read member upgrade lease: %w", err)
	}
	previous := ptr.Deref(current.Spec.HolderIdentity, "")
	previousExecutor := current.Annotations[executorAnnotation]
	now := time.Now()
	if previous != "" && (previous != holder || previousExecutor != executor) {
		if current.Spec.RenewTime == nil || current.Spec.LeaseDurationSeconds == nil ||
			now.Before(current.Spec.RenewTime.Add(time.Duration(*current.Spec.LeaseDurationSeconds)*time.Second)) || confirmExited == nil {
			return nil, ctx, &LeaseConflict{Holder: previous}
		}
		exited, err := confirmExited(ctx, previous, previousExecutor)
		if err != nil {
			return nil, ctx, fmt.Errorf("cannot verify that %q exited: %w", previous, err)
		}
		if !exited {
			return nil, ctx, &LeaseConflict{Holder: previous}
		}
	}
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[executorAnnotation] = executor
	current.Spec.HolderIdentity = ptr.To(holder)
	current.Spec.AcquireTime = ptr.To(metav1.NewMicroTime(now))
	current.Spec.RenewTime = ptr.To(metav1.NewMicroTime(now))
	current.Spec.LeaseDurationSeconds = ptr.To(int32(leaseDurationSeconds))
	if _, err := leases.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
		return nil, ctx, fmt.Errorf("claim member upgrade lease: %w", err)
	}
	work, cancel := context.WithCancelCause(ctx)
	guard := &Guard{leases: leases, holder: holder, executor: executor, cancel: cancel, done: make(chan struct{})}
	go guard.renew(work)
	return guard, work, nil
}

// Check fences each workload write against the current Lease holder.
func (g *Guard) Check(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	current, err := g.leases.Get(ctx, LeaseName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if ptr.Deref(current.Spec.HolderIdentity, "") != g.holder || current.Annotations[executorAnnotation] != g.executor {
		return &LeaseConflict{Holder: ptr.Deref(current.Spec.HolderIdentity, "")}
	}
	if current.Spec.RenewTime == nil || current.Spec.LeaseDurationSeconds == nil ||
		!time.Now().Before(current.Spec.RenewTime.Add(time.Duration(*current.Spec.LeaseDurationSeconds)*time.Second)) {
		return fmt.Errorf("member upgrade lease expired")
	}
	return nil
}

func (g *Guard) renew(ctx context.Context) {
	defer close(g.done)
	ticker := time.NewTicker(renewPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			call, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := g.update(call, false)
			cancel()
			if err != nil {
				g.cancel(fmt.Errorf("lost member upgrade lease: %w", err))
				return
			}
		}
	}
}

func (g *Guard) update(ctx context.Context, release bool) error {
	current, err := g.leases.Get(ctx, LeaseName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if ptr.Deref(current.Spec.HolderIdentity, "") != g.holder || current.Annotations[executorAnnotation] != g.executor {
		return &LeaseConflict{Holder: ptr.Deref(current.Spec.HolderIdentity, "")}
	}
	if release {
		current.Spec.HolderIdentity = nil
		delete(current.Annotations, executorAnnotation)
	} else {
		now := time.Now()
		if current.Spec.RenewTime == nil || current.Spec.LeaseDurationSeconds == nil ||
			!now.Before(current.Spec.RenewTime.Add(time.Duration(*current.Spec.LeaseDurationSeconds)*time.Second)) {
			return fmt.Errorf("member upgrade lease expired")
		}
		current.Spec.RenewTime = ptr.To(metav1.NewMicroTime(now))
	}
	_, err = g.leases.Update(ctx, current, metav1.UpdateOptions{})
	return err
}

// Release stops renewal and clears the Lease if the same executor still owns it.
func (g *Guard) Release(ctx context.Context) error {
	g.stop.Do(func() { g.cancel(context.Canceled) })
	<-g.done
	return g.update(ctx, true)
}
