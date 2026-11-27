package clusterupgrade

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	apps "k8s.io/client-go/kubernetes/typed/apps/v1"
)

// DeploymentTarget identifies the exact workload and the fields an image-only
// upgrade may change. It is persisted before mutation by the upgrade executor.
type DeploymentTarget struct {
	Name      string
	UID       types.UID
	Container string
	Before    Images
}

var ErrTargetChanged = errors.New("cluster Agent target changed during the upgrade")

func ReadDeploymentTarget(ctx context.Context, deployments apps.DeploymentInterface) (DeploymentTarget, error) {
	list, err := deployments.List(ctx, metav1.ListOptions{})
	if err != nil {
		return DeploymentTarget{}, err
	}
	var found DeploymentTarget
	for _, deployment := range list.Items {
		for _, container := range deployment.Spec.Template.Spec.Containers {
			args := slices.Concat(container.Command, container.Args)
			if _, _, ok := argument(args, "--mode", "cluster"); !ok {
				continue
			}
			if found.Name != "" || deployment.UID == "" || deployment.DeletionTimestamp != nil {
				return DeploymentTarget{}, errors.New("cannot identify exactly one current cluster Agent Deployment")
			}
			_, tools, ok := argument(container.Args, "--image", "")
			if !ok || container.Image == "" || tools == "" {
				return DeploymentTarget{}, errors.New("cannot identify the current cluster Agent and tools images")
			}
			found = DeploymentTarget{deployment.Name, deployment.UID, container.Name, Images{Agent: container.Image, Tools: tools}}
		}
	}
	if found.Name == "" {
		return found, errors.New("cluster Agent Deployment not found")
	}
	return found, nil
}

// WriteDeploymentImages updates only the Agent container image and its tools
// argument. Passing the snapshot and target in reverse performs rollback.
func WriteDeploymentImages(ctx context.Context, deployments apps.DeploymentInterface, target DeploymentTarget, from, to Images, checkLease func(context.Context) error) error {
	deployment, err := deployments.Get(ctx, target.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if deployment.UID != target.UID || deployment.DeletionTimestamp != nil {
		return ErrTargetChanged
	}
	container := slices.IndexFunc(deployment.Spec.Template.Spec.Containers, func(container corev1.Container) bool {
		return container.Name == target.Container
	})
	if container < 0 {
		return ErrTargetChanged
	}
	selected := &deployment.Spec.Template.Spec.Containers[container]
	if _, _, ok := argument(slices.Concat(selected.Command, selected.Args), "--mode", "cluster"); !ok {
		return ErrTargetChanged
	}
	index, tools, ok := argument(selected.Args, "--image", "")
	if !ok {
		return ErrTargetChanged
	}
	live := Images{Agent: selected.Image, Tools: tools}
	if live == to {
		return nil
	}
	if live != from {
		return ErrTargetChanged
	}
	selected.Image = to.Agent
	if strings.HasPrefix(selected.Args[index], "--image=") {
		selected.Args[index] = "--image=" + to.Tools
	} else {
		selected.Args[index] = to.Tools
	}
	if err := checkLease(ctx); err != nil {
		return fmt.Errorf("check cluster upgrade lease before writing Agent: %w", err)
	}
	_, err = deployments.Update(ctx, deployment, metav1.UpdateOptions{})
	return err
}

func DeploymentReady(deployment *appsv1.Deployment) bool {
	replicas := int32(1)
	if deployment.Spec.Replicas != nil {
		replicas = *deployment.Spec.Replicas
	}
	return replicas > 0 && deployment.Status.ObservedGeneration >= deployment.Generation &&
		deployment.Status.UpdatedReplicas == replicas &&
		deployment.Status.ReadyReplicas == replicas &&
		deployment.Status.AvailableReplicas == replicas &&
		deployment.Status.Replicas == replicas
}

// argument returns the index to replace and refuses repeated arguments.
func argument(args []string, key, expected string) (int, string, bool) {
	index := -1
	value := ""
	for i, arg := range args {
		prefix := key + "="
		at, candidate := -1, ""
		switch {
		case strings.HasPrefix(arg, prefix):
			at, candidate = i, strings.TrimPrefix(arg, prefix)
		case arg == key && i+1 < len(args) && !strings.HasPrefix(args[i+1], "--"):
			at, candidate = i+1, args[i+1]
		}
		if at >= 0 {
			if index >= 0 {
				return -1, "", false
			}
			index, value = at, candidate
		}
	}
	return index, value, index >= 0 && (expected == "" || value == expected)
}
