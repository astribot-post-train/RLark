package clusterupgrade

import (
	"context"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	apps "k8s.io/client-go/kubernetes/typed/apps/v1"
)

// Checkpoint identifies one immutable upgrade attempt. The actual Job must
// persist it before changing either workload so a replacement can resume.
type Checkpoint struct {
	Deployment  DeploymentTarget
	Target      Images
	NodeImage   string
	NodeAgent   NodeAgentTarget
	RollingBack bool
	Completed   bool
}

type NodeAgentTarget struct {
	ClusterUID string
	Before     string
}

// NodeAgentImages is the restricted management-plane adapter for the
// Cluster.spec.nodeAgents.image declaration. An empty Cluster UID means there are no
// managed node Agents. The implementation must also fence writes by Cluster UID.
type NodeAgentImages interface {
	Read(context.Context) (NodeAgentTarget, error)
	CompareAndSet(ctx context.Context, snapshot NodeAgentTarget, from, to string, rollback bool) error
	WaitReady(context.Context, NodeAgentTarget, string) error
}

// Checkpoints persists the current attempt by ClusterUpgradeJob UID.
type Checkpoints interface {
	Load(context.Context) (Checkpoint, bool, error)
	Save(context.Context, Checkpoint) error
}

type Executor struct {
	Deployments        apps.DeploymentInterface
	ExpectedDeployment string
	NodeAgents         NodeAgentImages
	Checkpoints        Checkpoints
	CheckLease         func(context.Context) error
	HealthTimeout      time.Duration
}

var ErrRolledBack = errors.New("upgrade failed and the previous images were restored")
var ErrRollbackFailed = errors.New("upgrade failed and rollback did not complete")

func (e Executor) Run(ctx context.Context, target Images, nodeImage string, autoRollback bool) error {
	checkpoint, exists, err := e.Checkpoints.Load(ctx)
	if err != nil {
		return fmt.Errorf("load upgrade checkpoint: %w", err)
	}
	if !exists {
		if err := e.CheckLease(ctx); err != nil {
			return err
		}
		deployment, err := ReadDeploymentTarget(ctx, e.Deployments)
		if err != nil {
			return err
		}
		if e.ExpectedDeployment != "" && e.ExpectedDeployment != deployment.Name {
			return ErrTargetChanged
		}
		nodes, err := e.NodeAgents.Read(ctx)
		if err != nil {
			return err
		}
		if (nodes.ClusterUID == "") != (nodeImage == "") {
			return errors.New("an explicit node Agent target is required exactly when managed node Agents are present")
		}
		checkpoint = Checkpoint{Deployment: deployment, Target: target, NodeImage: nodeImage, NodeAgent: nodes}
		if err := e.Checkpoints.Save(ctx, checkpoint); err != nil {
			return fmt.Errorf("persist upgrade checkpoint: %w", err)
		}
	} else if checkpoint.Target != target || checkpoint.NodeImage != nodeImage || (e.ExpectedDeployment != "" && checkpoint.Deployment.Name != e.ExpectedDeployment) {
		return errors.New("upgrade request differs from the persisted checkpoint")
	}
	if checkpoint.RollingBack {
		return e.rollback(ctx, checkpoint, nil)
	}

	if checkpoint.Completed {
		deployment, err := e.Deployments.Get(ctx, checkpoint.Deployment.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		ready, err := DeploymentAtTarget(deployment, checkpoint.Deployment, target)
		if err != nil {
			return err
		}
		if !ready {
			return ErrTargetChanged
		}
		if checkpoint.NodeAgent.ClusterUID != "" {
			health, cancel := context.WithTimeout(ctx, e.HealthTimeout)
			defer cancel()
			return e.NodeAgents.WaitReady(health, checkpoint.NodeAgent, checkpoint.NodeImage)
		}
		return nil
	}
	err = e.apply(ctx, checkpoint, checkpoint.Deployment.Before, target, checkpoint.NodeAgent.Before, checkpoint.NodeImage, false)
	if err == nil {
		checkpoint.Completed = true
		if err := e.Checkpoints.Save(ctx, checkpoint); err != nil {
			return fmt.Errorf("record completed upgrade: %w", err)
		}
		return nil
	}
	if !autoRollback || ctx.Err() != nil || errors.Is(err, ErrExecutorInterrupted) {
		return err
	}
	if leaseErr := e.CheckLease(ctx); leaseErr != nil {
		return errors.Join(err, ErrRollbackFailed, leaseErr)
	}
	checkpoint.RollingBack = true
	if saveErr := e.Checkpoints.Save(ctx, checkpoint); saveErr != nil {
		return errors.Join(err, ErrRollbackFailed, saveErr)
	}
	return e.rollback(ctx, checkpoint, err)
}

func (e Executor) rollback(ctx context.Context, checkpoint Checkpoint, upgradeErr error) error {
	if err := e.apply(ctx, checkpoint, checkpoint.Target, checkpoint.Deployment.Before, checkpoint.NodeImage, checkpoint.NodeAgent.Before, true); err != nil {
		return errors.Join(ErrRollbackFailed, upgradeErr, err)
	}
	return errors.Join(ErrRolledBack, upgradeErr)
}

func (e Executor) apply(ctx context.Context, checkpoint Checkpoint, from, to Images, oldNodeImage, nodeImage string, rollback bool) error {
	// Restore the managed declaration before waiting for the Deployment.
	// A degraded cluster Agent must not prevent the second image rollback.
	var nodeRollbackErr error
	if rollback && checkpoint.NodeAgent.ClusterUID != "" {
		if err := e.CheckLease(ctx); err != nil {
			return err
		}
		nodeRollbackErr = e.NodeAgents.CompareAndSet(ctx, checkpoint.NodeAgent, oldNodeImage, nodeImage, true)
	}
	if err := WriteDeploymentImages(ctx, e.Deployments, checkpoint.Deployment, from, to, e.CheckLease); err != nil {
		return errors.Join(nodeRollbackErr, err)
	}
	if nodeRollbackErr != nil {
		return nodeRollbackErr
	}
	health, cancel := context.WithTimeout(ctx, e.HealthTimeout)
	defer cancel()
	err := wait.PollUntilContextCancel(health, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		if err := e.CheckLease(ctx); err != nil {
			return false, err
		}
		deployment, err := e.Deployments.Get(ctx, checkpoint.Deployment.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return DeploymentAtTarget(deployment, checkpoint.Deployment, to)
	})
	if err != nil {
		return fmt.Errorf("wait for cluster Agent rollout: %w", err)
	}
	if checkpoint.NodeAgent.ClusterUID == "" {
		return nil
	}
	if err := e.CheckLease(health); err != nil {
		return err
	}
	if !rollback {
		if err := e.NodeAgents.CompareAndSet(health, checkpoint.NodeAgent, oldNodeImage, nodeImage, false); err != nil {
			return err
		}
	}
	return e.NodeAgents.WaitReady(health, checkpoint.NodeAgent, nodeImage)
}

// DeploymentAtTarget checks the original workload identity, image pair, and rollout status.
func DeploymentAtTarget(deployment *appsv1.Deployment, target DeploymentTarget, images Images) (bool, error) {
	if deployment.UID != target.UID || deployment.DeletionTimestamp != nil {
		return false, ErrTargetChanged
	}
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name == target.Container {
			_, tools, ok := argument(container.Args, "--image", "")
			if !ok || (Images{Agent: container.Image, Tools: tools}) != images {
				return false, ErrTargetChanged
			}
			return DeploymentReady(deployment), nil
		}
	}
	return false, ErrTargetChanged
}
