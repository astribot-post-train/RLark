package ownership

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	containerapi "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
)

func TestMatchesNativeRequiresUID(t *testing.T) {
	obj := &metav1.PartialObjectMetadata{}
	obj.Namespace, obj.Name, obj.UID = "tenant", "cluster", "uid-1"
	ref := containerapi.NativeObjectReference{Namespace: "tenant", Name: "cluster", UID: "uid-1"}
	if err := MatchesNative(ref, obj); err != nil {
		t.Fatal(err)
	}
	ref.UID = "uid-old"
	if err := MatchesNative(ref, obj); err == nil {
		t.Fatal("stale UID was accepted")
	}
}

func TestJobBindingMatchesTenant(t *testing.T) {
	job := &metav1.PartialObjectMetadata{}
	job.Namespace, job.Name, job.UID = "tenant", "job", "job-1"
	binding := &containerapi.ContainerJob{Spec: containerapi.ContainerJobSpec{Job: containerapi.NativeObjectReference{Namespace: "tenant", Name: "job", UID: "job-1"}, TenantID: "tenant-a", Runtime: "Raw"}}
	if err := JobBindingMatches(binding, job, "tenant-a"); err != nil {
		t.Fatal(err)
	}
	if err := JobBindingMatches(binding, job, "tenant-b"); err == nil {
		t.Fatal("cross-tenant binding was accepted")
	}
}
