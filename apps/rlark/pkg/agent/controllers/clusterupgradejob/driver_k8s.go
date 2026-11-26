package clusterupgradejob

import (
	"context"
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/clusterupgrade"
	"github.com/rlinf/rlark/apps/rlark/astribot/nodeagent"
)

// K8sDriver implements UpgradeDriver for a Kubernetes data plane.
// It runs upgrade/rollback as batchv1.Job and stores configs in ConfigMaps.
type K8sDriver struct {
	client            client.Client
	management        client.Reader
	namespace         string
	managementCluster string
	stateStore        StateStore
	policy            *clusterupgrade.JobPolicy
}

// NewK8sDriver creates a K8sDriver.
func NewK8sDriver(c client.Client, managementCluster string) *K8sDriver {
	return &K8sDriver{
		client:            c,
		namespace:         StateStoreNamespace,
		managementCluster: managementCluster,
		stateStore:        NewConfigMapStateStore(c),
	}
}

// StateStore returns the ConfigMap-backed StateStore.
func (d *K8sDriver) StateStore() StateStore {
	return d.stateStore
}

func (d *K8sDriver) SnapshotsLiveState() bool { return d.policy != nil }

func (d *K8sDriver) VerifyMissingJobSuccess(ctx context.Context, job *rlarkv1alpha1.ClusterUpgradeJob) (bool, error) {
	var object corev1.ConfigMap
	if err := d.client.Get(ctx, client.ObjectKey{Namespace: d.namespace, Name: clusterupgrade.CheckpointConfigMap}, &object); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	checkpoint, found, err := clusterupgrade.ReadCheckpoint(&object, string(job.UID))
	if err != nil {
		return false, fmt.Errorf("read upgrade checkpoint: %w", err)
	}
	if !found || !checkpoint.Completed || checkpoint.RollingBack || checkpoint.Deployment.UID == "" {
		return false, nil
	}
	if checkpoint.NodeAgent.ClusterUID == "" {
		images, err := clusterupgrade.ParseImages(job.Spec.ConfigPatch)
		if err != nil || checkpoint.Target != images || checkpoint.NodeImage != "" {
			return false, nil
		}
	} else if d.management == nil {
		return false, nil
	}
	var deployment appsv1.Deployment
	if err := d.client.Get(ctx, client.ObjectKey{Namespace: d.namespace, Name: checkpoint.Deployment.Name}, &deployment); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	ready, err := clusterupgrade.DeploymentAtTarget(&deployment, checkpoint.Deployment, checkpoint.Target)
	if errors.Is(err, clusterupgrade.ErrTargetChanged) {
		return false, nil
	}
	if !ready || err != nil || checkpoint.NodeAgent.ClusterUID == "" {
		return ready, err
	}
	var cluster rlarkv1alpha1.Cluster
	if err := d.management.Get(ctx, client.ObjectKey{Name: d.managementCluster}, &cluster); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	var daemonSet appsv1.DaemonSet
	if err := d.client.Get(ctx, client.ObjectKey{Namespace: d.namespace, Name: nodeagent.Name}, &daemonSet); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return clusterupgrade.VerifyManagedNodeCompletion(&cluster, &daemonSet, job, checkpoint), nil
}

func imageOnlyJobName(job *rlarkv1alpha1.ClusterUpgradeJob) string {
	return clusterupgrade.JobName(string(job.UID))
}

func ownsImageOnlyJob(child *rlarkv1alpha1.ClusterUpgradeJob, job *batchv1.Job) bool {
	return child.UID != "" && job.Name == imageOnlyJobName(child) &&
		job.Labels[clusterupgrade.ChildUIDLabel] == string(child.UID) &&
		job.Spec.Template.Spec.ServiceAccountName == clusterupgrade.JobServiceAccount
}

func (d *K8sDriver) attemptName(job *rlarkv1alpha1.ClusterUpgradeJob) string {
	if d.policy != nil {
		return imageOnlyJobName(job)
	}
	return "upgrade-" + job.Name
}

// PrepareConfig stores the merged deploy config as a ConfigMap.
// The configRef returned is the ConfigMap name.
func (d *K8sDriver) PrepareConfig(ctx context.Context, job *rlarkv1alpha1.ClusterUpgradeJob, config string) (string, error) {
	if d.policy != nil {
		if job.UID == "" {
			return "", fmt.Errorf("cluster upgrade job UID is required")
		}
		if err := d.policy.Validate(ctx, d.client, job.Spec.Image, config); err != nil {
			return "", err
		}
	}
	cmName := d.attemptName(job) + "-config"
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: d.namespace,
		},
		Data: map[string]string{
			"deploy-config.yaml": config,
		},
	}
	if d.policy != nil {
		cm.Immutable = ptr.To(true)
		cm.Labels = map[string]string{clusterupgrade.ChildUIDLabel: string(job.UID)}
	}
	if err := d.client.Create(ctx, cm); err != nil {
		// Tolerate AlreadyExists in case the agent restarted after creating
		// the ConfigMap but before recording the job status.
		if !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("create upgrade configmap: %w", err)
		}
		if d.policy != nil {
			var existing corev1.ConfigMap
			if err := d.client.Get(ctx, client.ObjectKeyFromObject(cm), &existing); err != nil {
				return "", err
			}
			if existing.Labels[clusterupgrade.ChildUIDLabel] != string(job.UID) || existing.Data["deploy-config.yaml"] != config || !ptr.Deref(existing.Immutable, false) {
				return "", fmt.Errorf("existing upgrade configuration does not belong to this immutable attempt")
			}
		}
	}
	return cmName, nil
}

// CleanupConfig deletes the per-upgrade ConfigMap. NotFound is success.
func (d *K8sDriver) CleanupConfig(ctx context.Context, job *rlarkv1alpha1.ClusterUpgradeJob) error {
	err := d.client.Delete(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      d.attemptName(job) + "-config",
			Namespace: d.namespace,
		},
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// StartUpgrade creates a batchv1.Job that runs the rlark-upgrade image.
func (d *K8sDriver) StartUpgrade(ctx context.Context, job *rlarkv1alpha1.ClusterUpgradeJob, rollbackImage, configRef string) (string, error) {
	timeout := job.Spec.TimeoutSeconds
	if timeout == 0 {
		timeout = 600
	}

	autoRollback := job.Spec.AutoRollback
	volumeMounts := []corev1.VolumeMount{{
		Name:      "deploy-config",
		MountPath: "/etc/rlark/deploy-config.yaml",
		SubPath:   "deploy-config.yaml",
	}}
	volumes := []corev1.Volume{{
		Name: "deploy-config",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: configRef},
			},
		},
	}}

	// Mount rollback config when a previous version exists.
	// Using Optional=true so the Pod can be scheduled even if rlark-state is absent.
	volumeMounts = append(volumeMounts, corev1.VolumeMount{
		Name:      "rollback-config",
		MountPath: "/etc/rlark/rollback-deploy-config.yaml",
		SubPath:   "rollback-deploy-config.yaml",
	})
	optional := true
	volumes = append(volumes, corev1.Volume{
		Name: "rollback-config",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: StateStoreName},
				Items: []corev1.KeyToPath{
					{Key: "deploy-config", Path: "rollback-deploy-config.yaml"},
				},
				Optional: &optional,
			},
		},
	})

	// Use a stable job name so that a restarted agent can find the existing job
	// instead of creating a duplicate.
	jobName := d.attemptName(job)
	k8sJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: d.namespace,
			Labels: map[string]string{
				"rlark.io/cluster-upgrade-job": job.Name,
				"rlark.io/cluster":             d.managementCluster,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: ptr.To(int32(3600)),
			BackoffLimit:            ptr.To(int32(0)),
			// Use 2x the user timeout so that health check failure (which uses
			// half the timeout) still leaves room for rollback before the
			// hard deadline kills the Pod and loses the exit code.
			ActiveDeadlineSeconds: ptr.To(timeout * 2), // 保证 job 不会因 ActiveDeadlineSeconds 而 terminating 拿不到退出码
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: "rlark-agent",
					Containers: []corev1.Container{{
						Name:  "upgrade",
						Image: job.Spec.Image,
						Env: []corev1.EnvVar{
							{Name: "TARGET_IMAGE", Value: job.Spec.Image},
							{Name: "ROLLBACK_IMAGE", Value: rollbackImage},
							{Name: "AUTO_ROLLBACK", Value: fmt.Sprintf("%v", autoRollback)},
							{Name: "HEALTH_CHECK_TIMEOUT", Value: fmt.Sprintf("%ds", timeout/2)},
						},
						VolumeMounts: volumeMounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}

	if d.policy != nil {
		k8sJob.Spec.Template.Spec.Volumes = volumes[:1]
		k8sJob.Spec.Template.Spec.Containers[0].VolumeMounts = volumeMounts[:1]
		clusterupgrade.ConfigureExecutor(k8sJob, *d.policy, string(job.UID), d.managementCluster)
	}

	if err := d.client.Create(ctx, k8sJob); err != nil {
		// If the job already exists (e.g. agent restarted after creating it),
		// return the existing job name so the caller can resume monitoring.
		if apierrors.IsAlreadyExists(err) {
			if d.policy != nil {
				var existing batchv1.Job
				if err := d.client.Get(ctx, client.ObjectKeyFromObject(k8sJob), &existing); err != nil {
					return "", err
				}
				if !ownsImageOnlyJob(job, &existing) {
					return "", fmt.Errorf("existing upgrade job does not belong to this attempt")
				}
			}
			return jobName, nil
		}
		return "", fmt.Errorf("create upgrade job: %w", err)
	}
	return jobName, nil
}

// CheckJobResult reads a batchv1.Job and returns its status plus the container
// exit code, so the caller can distinguish "upgrade failed, rollback succeeded"
// (exit code 2) from "upgrade failed, rollback also failed" (other non-zero).
func (d *K8sDriver) CheckJobResult(ctx context.Context, child *rlarkv1alpha1.ClusterUpgradeJob) (*JobResult, error) {
	jobName := child.Status.JobName
	var k8sJob batchv1.Job
	err := d.client.Get(ctx, types.NamespacedName{
		Namespace: d.namespace,
		Name:      jobName,
	}, &k8sJob)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return &JobResult{Status: JobStatusNotFound}, nil
		}
		return nil, err
	}

	if d.policy != nil && !ownsImageOnlyJob(child, &k8sJob) {
		return &JobResult{Status: JobStatusFailed, ExitCode: -1, Message: "the local upgrade Job does not belong to this attempt"}, nil
	}

	if k8sJob.Status.Active > 0 {
		return &JobResult{Status: JobStatusRunning}, nil
	}
	if k8sJob.Status.Succeeded > 0 {
		return &JobResult{Status: JobStatusSucceeded, ExitCode: 0}, nil
	}
	if k8sJob.Status.Failed > 0 {
		return d.failedJobResult(ctx, &k8sJob)
	}
	return &JobResult{Status: JobStatusRunning}, nil
}

// failedJobResult reads only the upgrade container of a Pod owned by this Job.
func (d *K8sDriver) failedJobResult(ctx context.Context, job *batchv1.Job) (*JobResult, error) {
	result := &JobResult{Status: JobStatusFailed, ExitCode: -1}
	if job.Spec.Selector == nil {
		return result, nil
	}
	selector, err := metav1.LabelSelectorAsSelector(job.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("build upgrade job selector: %w", err)
	}
	var pods corev1.PodList
	if err := d.client.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, fmt.Errorf("list upgrade job pods: %w", err)
	}
	var last *corev1.ContainerStateTerminated
	for _, pod := range pods.Items {
		owner := metav1.GetControllerOf(&pod)
		if owner == nil || owner.UID != job.UID || owner.Kind != "Job" || owner.APIVersion != batchv1.SchemeGroupVersion.String() {
			continue
		}
		for _, status := range pod.Status.ContainerStatuses {
			terminated := status.State.Terminated
			if status.Name == "upgrade" && terminated != nil && (last == nil || terminated.FinishedAt.After(last.FinishedAt.Time)) {
				last = terminated
			}
		}
	}
	if last != nil {
		result.ExitCode = last.ExitCode
		result.Message = clusterupgrade.PublicMessage(last.Message)
	}
	return result, nil
}

// ─── ConfigMap-backed StateStore ──────────────────────────────────────────────

// ConfigMapStateStore stores ClusterState as a ConfigMap in the local K8s cluster.
type ConfigMapStateStore struct {
	client    client.Client
	namespace string
	name      string
}

// NewConfigMapStateStore creates a StateStore backed by a ConfigMap.
func NewConfigMapStateStore(c client.Client) *ConfigMapStateStore {
	return &ConfigMapStateStore{
		client:    c,
		namespace: StateStoreNamespace,
		name:      StateStoreName,
	}
}

// Get reads the ConfigMap and returns the extracted ClusterState.
func (s *ConfigMapStateStore) Get(ctx context.Context) (*ClusterState, error) {
	var cm corev1.ConfigMap
	err := s.client.Get(ctx, types.NamespacedName{
		Namespace: s.namespace,
		Name:      s.name,
	}, &cm)
	if err != nil {
		return nil, err // caller checks apierrors.IsNotFound
	}
	return &ClusterState{
		Image:        cm.Data["image"],
		DeployConfig: cm.Data["deploy-config"],
	}, nil
}

// Save creates or updates the ConfigMap with the given ClusterState.
// An empty DeployConfig means the user did not provide a config patch for this
// upgrade, so the existing deploy-config is left unchanged (it represents the
// last explicitly provided configuration).
func (s *ConfigMapStateStore) Save(ctx context.Context, state *ClusterState) error {
	var cm corev1.ConfigMap
	err := s.client.Get(ctx, types.NamespacedName{
		Namespace: s.namespace,
		Name:      s.name,
	}, &cm)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get state configmap: %w", err)
		}
		cm = corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      s.name,
				Namespace: s.namespace,
			},
			Data: map[string]string{},
		}
		if state.DeployConfig != "" {
			cm.Data["deploy-config"] = state.DeployConfig
		}
		cm.Data["image"] = state.Image
		if createErr := s.client.Create(ctx, &cm); createErr != nil {
			return fmt.Errorf("create state configmap: %w", createErr)
		}
		return nil
	}

	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data["image"] = state.Image
	if state.DeployConfig != "" {
		cm.Data["deploy-config"] = state.DeployConfig
	}
	return s.client.Update(ctx, &cm)
}
