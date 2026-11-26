package clusterupgradejob

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/clusterupgrade"
	"github.com/rlinf/rlark/apps/rlark/pkg/log"
)

// RequeueInterval is the interval for requeuing during upgrade.
const RequeueInterval = 30 * time.Second

// pullReconciler reconciles ClusterUpgradeJob CRs.
type pullReconciler struct {
	c      *Controller
	log    logr.Logger
	driver UpgradeDriver
}

// newDriver returns the UpgradeDriver matching the agent's deployment mode.
// Docker and Raw modes are not yet implemented; the reconciler returns an
// error for unsupported modes so the ClusterUpgradeJob surfaces as failed.
func (r *pullReconciler) newDriver(ctx context.Context, job *rlarkv1alpha1.ClusterUpgradeJob) (UpgradeDriver, error) {
	if rlarkv1alpha1.AgentType(r.c.AgentType) != rlarkv1alpha1.AgentTypeKubernetes {
		return nil, fmt.Errorf("unsupported agent type %q for upgrade", r.c.AgentType)
	}
	driver := NewK8sDriver(r.c.LocalKubeClient, r.c.ManagementNamespace)
	if job.Status.Phase == rlarkv1alpha1.ClusterUpgradeJobUpgrading {
		// A running Job already selected its mode. Read its outcome even
		// when the member's policy was changed or temporarily unavailable.
		if job.Status.JobName == imageOnlyJobName(job) {
			driver.policy = &clusterupgrade.JobPolicy{}
		}
		return driver, nil
	}
	policy, err := clusterupgrade.ResolveJobPolicy(ctx, r.c.LocalKubeClient)
	if err != nil {
		return nil, err
	}
	if policy == nil {
		var cluster rlarkv1alpha1.Cluster
		if err := r.c.ManagementClient.Get(ctx, client.ObjectKey{Name: r.c.ManagementNamespace}, &cluster); err != nil {
			return nil, fmt.Errorf("verify stock upgrade eligibility: %w", err)
		}
		if err := clusterupgrade.ValidateStockTarget(cluster); err != nil {
			return nil, err
		}
	}
	driver.policy = policy
	return driver, nil
}

// Reconcile handles a ClusterUpgradeJob CR.
func (r *pullReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.log = log.FromContext(ctx)
	r.log.Info("reconciling cluster upgrade job", "name", req.Name)

	var clusterUpgradeJob rlarkv1alpha1.ClusterUpgradeJob
	if err := r.c.ManagementClient.Get(ctx, req.NamespacedName, &clusterUpgradeJob); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !r.shouldHandle(&clusterUpgradeJob) {
		r.log.V(1).Info("cluster upgrade job not for this cluster, skipping", "name", req.Name)
		return ctrl.Result{}, nil
	}

	if r.isCompleted(&clusterUpgradeJob) {
		r.log.V(1).Info("cluster upgrade job already completed", "name", req.Name)
		return ctrl.Result{}, nil
	}

	driver, err := r.newDriver(ctx, &clusterUpgradeJob)
	if err != nil {
		r.log.Error(err, "failed to create upgrade driver")
		return ctrl.Result{}, r.updateStatus(ctx, &clusterUpgradeJob, rlarkv1alpha1.ClusterUpgradeJobFailed, "", err.Error())
	}
	r.driver = driver

	switch clusterUpgradeJob.Status.Phase {
	case "", rlarkv1alpha1.ClusterUpgradeJobPending:
		return r.startUpgrade(ctx, &clusterUpgradeJob)
	case rlarkv1alpha1.ClusterUpgradeJobUpgrading:
		return r.monitorUpgrade(ctx, &clusterUpgradeJob)
	default:
		// Terminal state, nothing to do
		return ctrl.Result{}, nil
	}
}

func (r *pullReconciler) shouldHandle(job *rlarkv1alpha1.ClusterUpgradeJob) bool {
	return job.Labels["rlark.io/cluster"] == r.c.ManagementNamespace
}

func (r *pullReconciler) isCompleted(job *rlarkv1alpha1.ClusterUpgradeJob) bool {
	return job.Status.Phase == rlarkv1alpha1.ClusterUpgradeJobSucceeded ||
		job.Status.Phase == rlarkv1alpha1.ClusterUpgradeJobFailed ||
		job.Status.Phase == rlarkv1alpha1.ClusterUpgradeJobRolledBack
}

func (r *pullReconciler) startUpgrade(ctx context.Context, job *rlarkv1alpha1.ClusterUpgradeJob) (ctrl.Result, error) {
	r.log.Info("starting upgrade", "name", job.Name, "image", job.Spec.Image)

	var currentImage, currentConfig string
	state, err := r.driver.StateStore().Get(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.log.Info("previous upgrade record absent")
		} else {
			return ctrl.Result{}, r.updateStatus(ctx, job, rlarkv1alpha1.ClusterUpgradeJobFailed, "", fmt.Sprintf("get state: %v", err))
		}
	} else {
		currentImage = state.Image
		currentConfig = state.DeployConfig
	}

	newConfig := job.Spec.ConfigPatch
	if newConfig == "" && !r.driver.SnapshotsLiveState() {
		newConfig = currentConfig
	}

	configRef, err := r.driver.PrepareConfig(ctx, job, newConfig)
	if err != nil {
		return ctrl.Result{}, r.updateStatus(ctx, job, rlarkv1alpha1.ClusterUpgradeJobFailed, "", fmt.Sprintf("prepare config: %v", err))
	}

	effectiveJob := job
	if currentImage == "" && job.Spec.AutoRollback && !r.driver.SnapshotsLiveState() {
		r.log.Info("first upgrade, disabling auto rollback")
		jobCopy := job.DeepCopy()
		jobCopy.Spec.AutoRollback = false
		effectiveJob = jobCopy
	}

	jobName, err := r.driver.StartUpgrade(ctx, effectiveJob, currentImage, configRef)
	if err != nil {
		return ctrl.Result{}, r.updateStatus(ctx, job, rlarkv1alpha1.ClusterUpgradeJobFailed, "", fmt.Sprintf("start upgrade: %v", err))
	}

	r.log.Info("upgrade started", "name", job.Name, "jobName", jobName)
	return ctrl.Result{RequeueAfter: RequeueInterval}, r.updateStatus(ctx, job, rlarkv1alpha1.ClusterUpgradeJobUpgrading, jobName, "")
}

func (r *pullReconciler) monitorUpgrade(ctx context.Context, job *rlarkv1alpha1.ClusterUpgradeJob) (ctrl.Result, error) {
	r.log.Info("monitoring upgrade", "name", job.Name, "jobName", job.Status.JobName)

	result, err := r.driver.CheckJobResult(ctx, job.Status.JobName)
	if err != nil {
		return ctrl.Result{}, err
	}

	switch result.Status {
	case JobStatusRunning:
		r.log.V(1).Info("upgrade still running", "name", job.Name)
		return ctrl.Result{RequeueAfter: RequeueInterval}, nil

	case JobStatusSucceeded:
		return r.handleUpgradeSuccess(ctx, job)

	case JobStatusFailed:
		return r.handleUpgradeFailure(ctx, job, *result)

	case JobStatusNotFound:
		return r.handleJobNotFound(ctx, job)

	default:
		return ctrl.Result{RequeueAfter: RequeueInterval}, nil
	}
}

// handleJobNotFound determines the outcome when the upgrade job disappears
// before we can read its status (e.g. TTL-cleared or manually deleted).
func (r *pullReconciler) handleJobNotFound(ctx context.Context, job *rlarkv1alpha1.ClusterUpgradeJob) (ctrl.Result, error) {
	if r.driver.SnapshotsLiveState() {
		verified, err := r.driver.VerifyMissingJobSuccess(ctx, job)
		if err != nil {
			return ctrl.Result{}, err
		}
		if verified {
			return r.handleUpgradeSuccess(ctx, job)
		}
		return r.handleUpgradeFailure(ctx, job, JobResult{
			ExitCode: -1,
			Message:  "the upgrade Job disappeared and its outcome could not be verified for this attempt",
		})
	}
	r.log.Info("upgrade job not found, determining result from spec image", "name", job.Name)

	observed := job.Spec.Image

	state, err := r.driver.StateStore().Get(ctx)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, r.updateStatus(ctx, job, rlarkv1alpha1.ClusterUpgradeJobFailed, "", fmt.Sprintf("get state: %v", err))
	}

	stateImage := ""
	if state != nil {
		stateImage = state.Image
	}

	if stateImage == observed && observed != "" {
		r.log.Info("state image matches spec target, upgrade succeeded", "image", stateImage)
		return r.handleUpgradeSuccess(ctx, job)
	}

	r.log.Info("state image does not match observed target, upgrade failed", "state", stateImage, "observed", observed)
	return r.handleUpgradeFailure(ctx, job, JobResult{ExitCode: -1})
}

// handleUpgradeSuccess persists state and marks the job succeeded.
// A Save failure is returned as an error so the controller retries before
// declaring success.
func (r *pullReconciler) handleUpgradeSuccess(ctx context.Context, job *rlarkv1alpha1.ClusterUpgradeJob) (ctrl.Result, error) {
	r.log.Info("upgrade succeeded", "name", job.Name)

	if err := r.driver.StateStore().Save(ctx, &ClusterState{
		Image:        job.Spec.Image,
		DeployConfig: job.Spec.ConfigPatch,
	}); err != nil {
		r.log.Error(err, "save state")
		return ctrl.Result{}, fmt.Errorf("save state after upgrade: %w", err)
	}

	if err := r.driver.CleanupConfig(ctx, job); err != nil {
		r.log.Error(err, "cleanup config")
	}

	return ctrl.Result{}, r.updateStatus(ctx, job, rlarkv1alpha1.ClusterUpgradeJobSucceeded, "", "")
}

func (r *pullReconciler) handleUpgradeFailure(ctx context.Context, job *rlarkv1alpha1.ClusterUpgradeJob, result JobResult) (ctrl.Result, error) {
	r.log.Info("upgrade failed", "name", job.Name, "exitCode", result.ExitCode)

	// Rollback is handled inside the upgrade Pod itself (AUTO_ROLLBACK env).
	// Exit code 2 means "upgrade failed but rollback succeeded".
	// The agent does not create a separate rollback Job because the agent
	// itself may be unhealthy during the upgrade.
	if err := r.driver.CleanupConfig(ctx, job); err != nil {
		r.log.Error(err, "cleanup config")
	}

	if result.ExitCode == 2 {
		return ctrl.Result{}, r.updateStatus(ctx, job, rlarkv1alpha1.ClusterUpgradeJobRolledBack, "", upgradeFailureMessage("upgrade failed, rolled back successfully", result.Message))
	}
	return ctrl.Result{}, r.updateStatus(ctx, job, rlarkv1alpha1.ClusterUpgradeJobFailed, "", upgradeFailureMessage("upgrade job failed", result.Message))
}

func upgradeFailureMessage(summary, detail string) string {
	if detail == "" {
		return summary
	}
	return summary + ": " + detail
}

// updateStatus updates the ClusterUpgradeJob status.
func (r *pullReconciler) updateStatus(ctx context.Context, job *rlarkv1alpha1.ClusterUpgradeJob, phase rlarkv1alpha1.ClusterUpgradeJobPhase, jobName, message string) error {
	return r.updateStatusInternal(ctx, job, phase, jobName, "", message)
}

// updateStatusInternal updates the ClusterUpgradeJob status.
func (r *pullReconciler) updateStatusInternal(ctx context.Context, job *rlarkv1alpha1.ClusterUpgradeJob, phase rlarkv1alpha1.ClusterUpgradeJobPhase, jobName, rollbackJobName, message string) error {
	job.Status.Phase = phase
	if jobName != "" {
		job.Status.JobName = jobName
	}
	if rollbackJobName != "" {
		job.Status.RollbackJobName = rollbackJobName
	}
	job.Status.Message = message

	if job.Status.StartTime == nil && phase == rlarkv1alpha1.ClusterUpgradeJobUpgrading {
		job.Status.StartTime = &metav1.Time{Time: time.Now()}
	}

	if phase == rlarkv1alpha1.ClusterUpgradeJobSucceeded ||
		phase == rlarkv1alpha1.ClusterUpgradeJobFailed ||
		phase == rlarkv1alpha1.ClusterUpgradeJobRolledBack {
		job.Status.CompletionTime = &metav1.Time{Time: time.Now()}
	}

	return r.c.ManagementClient.Status().Update(ctx, job)
}
