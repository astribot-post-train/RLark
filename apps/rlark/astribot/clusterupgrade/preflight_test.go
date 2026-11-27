package clusterupgrade

import (
	"strings"
	"testing"

	rlark "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPreflightKeepsStockTargetsAndRefusesUnsafeFallback(t *testing.T) {
	standard := rlark.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "standard", Annotations: map[string]string{DriverAnnotation: DriverStock}, Labels: map[string]string{"role": "worker"}}}
	astribot := rlark.Cluster{ObjectMeta: metav1.ObjectMeta{
		Name: "astribot", Labels: map[string]string{"role": "robot"},
		Annotations: map[string]string{DriverAnnotation: DriverImageOnly},
	}}
	clusters := []rlark.Cluster{standard, astribot}
	spec := rlark.UpgradeJobSpec{
		Image:           "stock-upgrader",
		ClusterNames:    []string{"standard"},
		ClusterSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"role": "robot"}},
	}
	if err := Preflight(spec, clusters); err != nil {
		t.Fatalf("named stock target should win over selector: %v", err)
	}
	spec.ClusterNames = nil
	spec.ConfigPatch = imagesConfig + "  image-pull-policy: Never\n"
	if err := Preflight(spec, clusters); err == nil || !strings.Contains(err.Error(), "kubernetes.image-pull-policy") {
		t.Fatalf("forbidden field was not reported: %v", err)
	}
	spec.ConfigPatch = imagesConfig
	if err := Preflight(spec, clusters); err == nil || !strings.Contains(err.Error(), "requires an Astribot BatchDeployConfig") {
		t.Fatalf("the image-only target could reach the stock installer: %v", err)
	}
}

func TestPreflightRejectsLegacyAndManagedTargets(t *testing.T) {
	spec := rlark.UpgradeJobSpec{Image: "stock-upgrader", ClusterNames: []string{"member"}}
	member := rlark.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "member"}}
	if err := Preflight(spec, []rlark.Cluster{member}); err == nil || !strings.Contains(err.Error(), "explicitly declare") {
		t.Fatalf("unmarked target was not rejected: %v", err)
	}
	member.Annotations = map[string]string{DriverAnnotation: DriverStock}
	member.Spec.NodeAgents = &rlark.ClusterNodeAgents{}
	if err := Preflight(spec, []rlark.Cluster{member}); err == nil || !strings.Contains(err.Error(), "managed node Agents") {
		t.Fatalf("managed target was not rejected: %v", err)
	}
}
