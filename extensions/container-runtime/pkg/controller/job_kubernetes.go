package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/core/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/taskcontract"
	"github.com/rlinf/rlark/api/metadata"
	native "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/host"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	kubernetesTaskProtection   = "containers.rlark.io/kubernetes-task-protection"
	targetClusterUID           = metadata.TargetClusterUIDAnnotation
	targetJobGeneration        = "containers.rlark.io/job-generation"
	kubernetesResultAnnotation = "containers.rlark.io/kubernetes-result"
)

type kubernetesResult struct {
	TaskUID    string           `json:"taskUID"`
	Generation int64            `json:"generation"`
	Phase      native.TaskPhase `json:"phase"`
}

func readKubernetesResult(task *native.Task) (*kubernetesResult, error) {
	data, exists := task.Annotations[kubernetesResultAnnotation]
	if !exists {
		return nil, nil
	}
	var result kubernetesResult
	if json.Unmarshal([]byte(data), &result) != nil || task.UID == "" || !task.Spec.Suspended || result.TaskUID != string(task.UID) || result.Generation < 1 || result.Generation > task.Generation || task.Generation > result.Generation+1 || (result.Phase != native.TaskPhaseSucceeded && result.Phase != native.TaskPhaseFailed) {
		return nil, invalidBinding("Kubernetes result receipt does not match Task identity or generation")
	}
	return &result, nil
}

// observeKubernetes saves the current native result before requesting durable
// suspension. Native Host reports current Stopped only after runtime cleanup;
// its finalizer remains owned by that Host until later deletion.
func (r JobReservationReconciler) observeKubernetes(ctx context.Context, task *native.Task) (native.TaskPhase, bool, error) {
	result, err := readKubernetesResult(task)
	if err != nil {
		return "", false, err
	}
	if result != nil {
		return result.Phase, task.Spec.Suspended && kubernetesObserved(task) && task.Status.Phase == native.TaskPhaseStopped, nil
	}
	if !kubernetesObserved(task) {
		return native.TaskPhasePending, false, nil
	}
	if task.Status.Phase != native.TaskPhaseSucceeded && task.Status.Phase != native.TaskPhaseFailed {
		return task.Status.Phase, false, nil
	}
	before := task.DeepCopy()
	data, err := json.Marshal(kubernetesResult{TaskUID: string(task.UID), Generation: task.Generation, Phase: task.Status.Phase})
	if err != nil {
		return "", false, err
	}
	task.Annotations[kubernetesResultAnnotation] = string(data)
	task.Spec.Suspended = true
	err = r.Client.Patch(ctx, task, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	return before.Status.Phase, false, err
}

func executionTaskMatches(task, wanted *native.Task) bool {
	if task.Spec.Runtime.Kind != "Kubernetes" {
		return taskcontract.EqualSpec(task.Spec, wanted.Spec)
	}
	result, err := readKubernetesResult(task)
	if err != nil {
		return false
	}
	spec := task.Spec.DeepCopy()
	if result != nil {
		spec.Suspended = wanted.Spec.Suspended
	}
	return taskcontract.EqualSpec(*spec, wanted.Spec)
}

// kubernetesTargets resolves trusted placement from current Cluster instances.
// A namespace is never accepted from a workload or inferred from labels.
func (r ContainerJobReconciler) kubernetesTargets(ctx context.Context, b *ext.ContainerJob, job *native.Job) (map[string]string, error) {
	namespaces, err := taskcontract.KubernetesTargets(ctx, r.Client, b, job)
	if errors.Is(err, taskcontract.ErrInvalidTarget) {
		return nil, fmt.Errorf("%w: %w", errInvalidBinding, err)
	}
	return namespaces, err
}

func kubernetesObserved(task *native.Task) bool {
	return task.UID != "" && task.Generation > 0 && task.Status.ObservedGeneration == task.Generation
}

// retireKubernetes retains our identity protection until the native Host has
// removed its own cleanup finalizer. A delete request is not cleanup proof.
func (r JobReservationReconciler) retireKubernetes(ctx context.Context, task *native.Task) (bool, error) {
	if task.DeletionTimestamp.IsZero() {
		return false, client.IgnoreNotFound(r.Client.Delete(ctx, task, client.Preconditions{UID: &task.UID, ResourceVersion: &task.ResourceVersion}))
	}
	if controllerutil.ContainsFinalizer(task, host.TaskFinalizer) {
		return false, nil
	}
	if controllerutil.ContainsFinalizer(task, kubernetesTaskProtection) {
		before := task.DeepCopy()
		controllerutil.RemoveFinalizer(task, kubernetesTaskProtection)
		if err := r.Client.Patch(ctx, task, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return false, err
		}
	}
	return false, nil // next read must observe the exact child absent
}

func kubernetesTaskMatches(task, wanted *native.Task, generation int64) bool {
	return task.Annotations[targetClusterUID] == wanted.Annotations[targetClusterUID] && task.Annotations[targetJobGeneration] == strconv.FormatInt(generation, 10) && controllerutil.ContainsFinalizer(task, kubernetesTaskProtection)
}
