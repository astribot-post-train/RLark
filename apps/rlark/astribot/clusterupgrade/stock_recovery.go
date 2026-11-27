package clusterupgrade

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const stockCheckpointKey = "stock-upgrade-checkpoint"

type stockPhase string

const (
	stockApplying    stockPhase = "applying"
	stockRollingBack stockPhase = "rolling-back"
	stockRolledBack  stockPhase = "rolled-back"
	stockCompleted   stockPhase = "completed"
	stockFailed      stockPhase = "failed"
)

var ErrStockRecoveryUnverified = errors.New("stock upgrade recovery cannot verify the original attempt or rollback baseline")

type stockCheckpoint struct {
	StateUID, ChildUID, JobUID, PodUID, Image string
	TargetHash, BaselineHash                  string
	Rollback                                  bool
	Phase                                     stockPhase
}

// StockOperations is the native stock installer behind the durable executor.
type StockOperations interface {
	Upgrade(context.Context) error
	Rollback(context.Context) error
}

func stockHash(values ...string) string {
	data, _ := json.Marshal(values)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// Run persists the direction before invoking the native installer. A different
// Pod under the same Job restores the original baseline after interruption.
// It never infers a rollback configuration from partially upgraded objects.
func (s *LockedStock) Run(requested string, fromState, autoRollback bool, prepare func(string, string) (StockOperations, error)) error {
	ctx := s.Context
	var state *corev1.ConfigMap
	if err := retryMemberAPI(ctx, func(call context.Context) error {
		var err error
		state, err = s.ReadState(call)
		return err
	}); err != nil {
		if apierrors.IsNotFound(err) {
			return errors.Join(ErrStockRecoveryUnverified, err)
		}
		return stockInterrupted(ctx, err)
	}
	if state.UID == "" || s.jobUID == "" || s.podUID == "" {
		return ErrStockRecoveryUnverified
	}
	effective := requested
	if fromState {
		effective = state.Data["deploy-config"]
	}
	baseline, oldImage := state.Data["deploy-config"], state.Data["image"]
	targetHash := stockHash(effective)
	baselineHash := stockHash(oldImage, baseline)
	var saved stockCheckpoint
	switch record := state.Data[stockCheckpointKey]; {
	case record == "":
		if s.mutationRecorded {
			return ErrStockRecoveryUnverified
		}
	case json.Unmarshal([]byte(record), &saved) != nil || saved.StateUID != string(state.UID) || saved.ChildUID == "" || saved.JobUID == "" || saved.PodUID == "" ||
		saved.Image == "" || saved.TargetHash == "" || saved.BaselineHash == "":
		return ErrStockRecoveryUnverified
	default:
		switch saved.Phase {
		case stockApplying, stockRollingBack, stockRolledBack, stockCompleted, stockFailed:
		default:
			return ErrStockRecoveryUnverified
		}
	}
	sameAttempt := saved.ChildUID == s.childUID && saved.ChildUID != ""
	if sameAttempt {
		if saved.JobUID != s.jobUID || saved.Image != s.image || saved.TargetHash != targetHash {
			return ErrStockRecoveryUnverified
		}
		if saved.Phase == stockCompleted {
			if state.Data[StockAttemptKey] != s.childUID || state.Data["image"] != s.image ||
				stockHash(state.Data["deploy-config"]) != targetHash {
				return ErrStockRecoveryUnverified
			}
			return nil
		}
		if saved.BaselineHash != baselineHash {
			return ErrStockRecoveryUnverified
		}
		if saved.Phase == stockRolledBack {
			return ErrRolledBack
		}
		if saved.Phase == stockFailed {
			return ErrStockRecoveryUnverified
		}
	} else if saved.Phase == stockApplying || saved.Phase == stockRollingBack {
		// A new attempt may reapply the interrupted target or the verified
		// baseline after the original Job has stopped.
		matches := saved.TargetHash == targetHash || (oldImage != "" && baseline != "" && effective == baseline)
		if saved.BaselineHash != baselineHash || !matches || s.previousJobExited == nil {
			return ErrStockRecoveryUnverified
		}
		exited, err := s.previousJobExited(ctx, saved)
		if err != nil {
			return stockInterrupted(ctx, err)
		}
		if !exited {
			return ErrStockRecoveryUnverified
		}
	}

	resume := sameAttempt
	if resume && (!saved.Rollback || oldImage == "" || baseline == "") {
		return ErrStockRecoveryUnverified
	}
	if !sameAttempt {
		saved = stockCheckpoint{StateUID: string(state.UID), ChildUID: s.childUID, JobUID: s.jobUID, PodUID: s.podUID,
			Image: s.image, TargetHash: targetHash, BaselineHash: baselineHash, Rollback: autoRollback && oldImage != "" && baseline != "", Phase: stockApplying}
	}
	runner, err := prepare(effective, baseline)
	if err != nil {
		return err
	}
	write := func(phase stockPhase) error {
		saved.Phase = phase
		return retryMemberAPI(ctx, func(call context.Context) error {
			encoded, err := json.Marshal(saved)
			if err != nil {
				return err
			}
			next := state.DeepCopy()
			if next.Data == nil {
				next.Data = map[string]string{}
			}
			next.Data[stockCheckpointKey] = string(encoded)
			if phase == stockCompleted {
				next.Data["image"], next.Data["deploy-config"], next.Data[StockAttemptKey] = s.image, effective, s.childUID
			}
			if err := s.Check(call); err != nil {
				return err
			}
			updated, err := s.state.Update(call, next, metav1.UpdateOptions{})
			if err != nil {
				// A timed-out update may have succeeded before its response was lost.
				current, readErr := s.ReadState(call)
				if readErr != nil || current.UID != state.UID || current.Data[stockCheckpointKey] != string(encoded) ||
					current.Data["image"] != next.Data["image"] || current.Data["deploy-config"] != next.Data["deploy-config"] ||
					current.Data[StockAttemptKey] != next.Data[StockAttemptKey] {
					return errors.Join(err, readErr)
				}
				updated = current
			}
			state = updated
			return nil
		})
	}

	markMutation := func() error {
		if s.beforeMutation == nil {
			return nil
		}
		return stockInterrupted(ctx, s.beforeMutation(ctx))
	}
	if !resume {
		if err := write(stockApplying); err != nil {
			return stockInterrupted(ctx, err)
		}
		if err := markMutation(); err != nil {
			return err
		}
		err = runner.Upgrade(ctx)
		if err == nil {
			return stockInterrupted(ctx, write(stockCompleted))
		}
		if interrupted := stockInterrupted(ctx, err); errors.Is(interrupted, ErrExecutorInterrupted) {
			return interrupted
		}
		if !saved.Rollback {
			return errors.Join(err, stockInterrupted(ctx, write(stockFailed)))
		}
	}
	if writeErr := write(stockRollingBack); writeErr != nil {
		return stockInterrupted(ctx, errors.Join(ErrRollbackFailed, err, writeErr))
	}
	if err := markMutation(); err != nil {
		return err
	}
	if rollbackErr := runner.Rollback(ctx); rollbackErr != nil {
		return stockInterrupted(ctx, errors.Join(ErrRollbackFailed, err, rollbackErr))
	}
	if writeErr := write(stockRolledBack); writeErr != nil {
		return stockInterrupted(ctx, errors.Join(ErrRollbackFailed, err, writeErr))
	}
	return errors.Join(ErrRolledBack, err)
}

func stockInterrupted(ctx context.Context, err error) error {
	if context.Cause(ctx) != nil || errors.Is(err, ErrLeaseLost) || errors.Is(err, ErrExecutorInterrupted) || temporaryAPIError(err) {
		return errors.Join(ErrExecutorInterrupted, context.Cause(ctx), err)
	}
	return err
}

// RetainForRecovery stops renewing without dropping a recovering Job's slot.
func (s *LockedStock) RetainForRecovery() { s.stop() }
