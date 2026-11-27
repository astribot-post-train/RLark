package clusterupgrade

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// ConfigureExecutor applies the member policy to the launcher's single-container
// image-only Job. The launcher attaches only the immutable DeployConfig volume.
func ConfigureExecutor(job *batchv1.Job, policy JobPolicy, childUID, childName, cluster string) {
	metav1.SetMetaDataLabel(&job.ObjectMeta, ChildUIDLabel, childUID)
	configureExecutorRetry(job)
	if job.Spec.ActiveDeadlineSeconds != nil {
		job.Spec.ActiveDeadlineSeconds = ptr.To(*job.Spec.ActiveDeadlineSeconds + executorHandoffSeconds)
	}
	pod := &job.Spec.Template.Spec
	executor := &pod.Containers[0]
	pod.ServiceAccountName = JobServiceAccount
	executor.VolumeMounts[0].ReadOnly = true
	mode := ClusterOnlyMode
	if policy.Mode == ManagedNodeMode {
		mode = ManagedNodeMode
	}
	executor.Env = append(executor.Env,
		corev1.EnvVar{Name: "RLARK_UPGRADE_MODE", Value: mode},
		corev1.EnvVar{Name: "RLARK_UPGRADE_JOB_UID", Value: childUID},
		corev1.EnvVar{Name: "RLARK_UPGRADE_DEPLOYMENT", Value: policy.Deployment},
		corev1.EnvVar{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		corev1.EnvVar{Name: "POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}},
	)
	if mode != ManagedNodeMode {
		return
	}
	const volume = "node-image-broker-token"
	pod.Volumes = append(pod.Volumes, corev1.Volume{
		Name: volume, VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{
				ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
					Audience: BrokerAudience, ExpirationSeconds: ptr.To(int64(600)), Path: "token",
				},
			}}},
		},
	})
	executor.VolumeMounts = append(executor.VolumeMounts, corev1.VolumeMount{Name: volume, MountPath: BrokerTokenMount, ReadOnly: true})
	executor.Env = append(executor.Env,
		corev1.EnvVar{Name: "RLARK_UPGRADE_CLUSTER", Value: cluster},
		corev1.EnvVar{Name: "RLARK_UPGRADE_CHILD_NAME", Value: childName},
	)
}

// ConfigureLockedStock keeps the stock ServiceAccount and enables checkpoint recovery.
// Only the locally pinned executor can be trusted to honor the lock.
func ConfigureLockedStock(job *batchv1.Job, childUID string, useStateConfig bool) {
	metav1.SetMetaDataLabel(&job.ObjectMeta, ChildUIDLabel, childUID)
	configureExecutorRetry(job)
	if job.Spec.ActiveDeadlineSeconds != nil {
		// Reserve a rollout timeout for rollback after the forward attempt, and
		// time for two replacement Pods to wait out the previous Lease.
		original := *job.Spec.ActiveDeadlineSeconds
		job.Spec.ActiveDeadlineSeconds = ptr.To(original + original/2 + 2*executorHandoffSeconds)
	}
	container := &job.Spec.Template.Spec.Containers[0]
	container.Env = append(container.Env,
		corev1.EnvVar{Name: "RLARK_UPGRADE_MODE", Value: DriverStock},
		corev1.EnvVar{Name: "RLARK_UPGRADE_STOCK_LOCK", Value: "1"},
		corev1.EnvVar{Name: "RLARK_UPGRADE_JOB_UID", Value: childUID},
		corev1.EnvVar{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		corev1.EnvVar{Name: "POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}},
	)
	if useStateConfig {
		container.Env = append(container.Env, corev1.EnvVar{Name: "RLARK_UPGRADE_STOCK_CONFIG_FROM_STATE", Value: "1"})
	}
}

func configureExecutorRetry(job *batchv1.Job) {
	// Retry a lost/disrupted executor, but preserve structured terminal
	// results (including completed rollback) without replaying the work.
	job.Spec.BackoffLimit = ptr.To(int32(2))
	job.Spec.PodReplacementPolicy = ptr.To(batchv1.Failed)
	job.Spec.PodFailurePolicy = &batchv1.PodFailurePolicy{Rules: []batchv1.PodFailurePolicyRule{
		{Action: batchv1.PodFailurePolicyActionFailJob, OnExitCodes: &batchv1.PodFailurePolicyOnExitCodesRequirement{
			ContainerName: ptr.To("upgrade"), Operator: batchv1.PodFailurePolicyOnExitCodesOpNotIn, Values: []int32{137, 143},
		}},
	}}
}
