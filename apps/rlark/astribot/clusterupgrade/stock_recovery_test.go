package clusterupgrade

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type stockTestOperations struct {
	upgrade, rollback func(context.Context) error
}

func (o stockTestOperations) Upgrade(ctx context.Context) error {
	if o.upgrade != nil {
		return o.upgrade(ctx)
	}
	return nil
}

func (o stockTestOperations) Rollback(ctx context.Context) error {
	if o.rollback != nil {
		return o.rollback(ctx)
	}
	return nil
}

func TestStockRecoveryRequiresOriginalAttemptAndBaseline(t *testing.T) {
	for _, tt := range []struct {
		name                     string
		autoRollback, noBaseline bool
		change                   func(*LockedStock, *corev1.ConfigMap)
		want                     error
	}{
		{name: "replacement rolls back", autoRollback: true, want: ErrRolledBack},
		{name: "rollback disabled", want: ErrStockRecoveryUnverified},
		{name: "no baseline on first upgrade", autoRollback: true, noBaseline: true, want: ErrStockRecoveryUnverified},
		{name: "different Job UID", autoRollback: true, change: func(s *LockedStock, _ *corev1.ConfigMap) {
			s.jobUID = "replacement-job"
		}, want: ErrStockRecoveryUnverified},
		{name: "different child", autoRollback: true, change: func(s *LockedStock, _ *corev1.ConfigMap) {
			s.childUID = "other-child"
		}, want: ErrStockRecoveryUnverified},
		{name: "recreated state without checkpoint", autoRollback: true, change: func(_ *LockedStock, state *corev1.ConfigMap) {
			state.UID = "replacement-state"
			delete(state.Data, stockCheckpointKey)
		}, want: ErrStockRecoveryUnverified},
		{name: "recreated state", autoRollback: true, change: func(_ *LockedStock, state *corev1.ConfigMap) {
			state.UID = "replacement-state"
		}, want: ErrStockRecoveryUnverified},
		{name: "changed rollback config", autoRollback: true, change: func(_ *LockedStock, state *corev1.ConfigMap) {
			state.Data["deploy-config"] = "unexpected"
		}, want: ErrStockRecoveryUnverified},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			data := map[string]string{"image": "previous", "deploy-config": "baseline", "other": "preserve"}
			if tt.noBaseline {
				delete(data, "image")
				delete(data, "deploy-config")
			}
			member := fake.NewClientset(&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: Namespace, Name: StockStateName, UID: "state"},
				Data:       data,
			})
			stock := &LockedStock{Context: ctx, Check: func(context.Context) error { return nil },
				state: member.CoreV1().ConfigMaps(Namespace), childUID: "child", jobUID: "job", podUID: "pod-1", image: "next"}
			err := stock.Run("target", false, tt.autoRollback, func(_, _ string) (StockOperations, error) {
				return stockTestOperations{upgrade: func(context.Context) error { return ErrExecutorInterrupted }}, nil
			})
			if !errors.Is(err, ErrExecutorInterrupted) {
				t.Fatalf("original execution: %v", err)
			}
			stock.podUID = "pod-2"
			stock.replacement = true
			state, err := stock.ReadState(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if tt.change != nil {
				tt.change(stock, state)
				if _, err := stock.state.Update(ctx, state, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			err = stock.Run("target", false, tt.autoRollback, func(target, baseline string) (StockOperations, error) {
				if target != "target" || baseline != "baseline" {
					t.Fatalf("prepare received %q and %q", target, baseline)
				}
				return stockTestOperations{
					upgrade:  func(context.Context) error { t.Fatal("replayed upgrade"); return nil },
					rollback: func(context.Context) error { calls++; return nil },
				}, nil
			})
			if !errors.Is(err, tt.want) || (calls == 1) != errors.Is(tt.want, ErrRolledBack) {
				t.Fatalf("replacement: calls=%d, error=%v, expected %v", calls, err, tt.want)
			}
			result, err := stock.ReadState(ctx)
			if err != nil || result.Data["image"] != data["image"] || result.Data["other"] != "preserve" {
				t.Fatalf("baseline metadata changed: %v, %v", result, err)
			}
			if errors.Is(tt.want, ErrStockRecoveryUnverified) && result.Data[stockCheckpointKey] != state.Data[stockCheckpointKey] {
				t.Fatal("unverified recovery rewrote the original checkpoint")
			}
		})
	}
}
