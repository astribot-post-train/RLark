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
