package clusterupgrade

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	core "k8s.io/client-go/kubernetes/typed/core/v1"
)

const CheckpointConfigMap = "rlark-agent-upgrade-checkpoint"

// ConfigMapCheckpoint binds persisted state to the immutable child Job UID.
// The member lease's exclusive holder is the only caller allowed to save.
type ConfigMapCheckpoint struct {
	ConfigMaps core.ConfigMapInterface
	JobUID     string
	CheckLease func(context.Context) error
}

func (s ConfigMapCheckpoint) Load(ctx context.Context) (Checkpoint, bool, error) {
	object, err := s.ConfigMaps.Get(ctx, CheckpointConfigMap, metav1.GetOptions{})
	if err != nil {
		return Checkpoint{}, false, err
	}
	if object.Data["jobUID"] != s.JobUID {
		return Checkpoint{}, false, nil
	}
	var value Checkpoint
	err = json.Unmarshal([]byte(object.Data["snapshot.json"]), &value)
	return value, err == nil, err
}

func (s ConfigMapCheckpoint) Save(ctx context.Context, state Checkpoint) error {
	if s.JobUID == "" {
		return fmt.Errorf("upgrade job UID is required")
	}
	object, err := s.ConfigMaps.Get(ctx, CheckpointConfigMap, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if object.Data == nil {
		object.Data = make(map[string]string)
	}
	if s.JobUID == object.Data["jobUID"] {
		var before Checkpoint
		if err := json.Unmarshal([]byte(object.Data["snapshot.json"]), &before); err != nil {
			return err
		}
		if before.Deployment != state.Deployment || before.Target != state.Target || before.NodeAgent != state.NodeAgent ||
			(before.RollingBack && !state.RollingBack) {
			return fmt.Errorf("upgrade checkpoint cannot be replaced")
		}
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return err
	}
	object.Data["jobUID"] = s.JobUID
	object.Data["snapshot.json"] = string(payload)
	if err := s.CheckLease(ctx); err != nil {
		return err
	}
	_, err = s.ConfigMaps.Update(ctx, object, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) {
		return fmt.Errorf("upgrade checkpoint changed while saving: %w", err)
	}
	return err
}
