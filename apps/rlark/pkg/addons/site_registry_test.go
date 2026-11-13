package addons

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func TestSiteRegistryRendersBothRoles(t *testing.T) {
	a, ok := Registry.Get("site-registry")
	if !ok {
		t.Fatal("site-registry not in catalog")
	}
	values := map[string]string{"image": "registry@sha256:x", "s3Bucket": "b", "s3Region": "r", "s3Endpoint": "https://s3", "s3RootDirectory": "/root"}

	values["listen"] = "0.0.0.0:5443"
	m, err := a.Render(values, "ns", "office-registry", "u")
	if err != nil {
		t.Fatal(err)
	}
	var config corev1.ConfigMap
	if err := yaml.Unmarshal(m[0].Raw, &config); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(config.Data["config.yml"], "htpasswd") || strings.Contains(config.Data["config.yml"], "readonly") {
		t.Fatalf("registry config:\n%s", config.Data["config.yml"])
	}
	var deployment appsv1.Deployment
	if err := yaml.UnmarshalStrict(m[1].Raw, &deployment); err != nil {
		t.Fatalf("%v\n%s", err, m[1].Raw)
	}
	spec := deployment.Spec.Template.Spec
	if deployment.Kind != "Deployment" || spec.NodeSelector["rlark.io/site-registry"] != "true" || len(spec.Volumes) != 2 || spec.Containers[0].ReadinessProbe.TCPSocket.Port.IntValue() != 5443 {
		t.Fatalf("registry workload:\n%s", m[1].Raw)
	}

	values["role"], values["listen"] = "mirror", "127.0.0.1:5444"
	m, err = a.Render(values, "ns", "cloud-mirror", "u")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(m[0].Raw, &config); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(config.Data["config.yml"], "htpasswd") || !strings.Contains(config.Data["config.yml"], "readonly") {
		t.Fatalf("mirror config:\n%s", config.Data["config.yml"])
	}
	var daemonSet appsv1.DaemonSet
	if err := yaml.UnmarshalStrict(m[1].Raw, &daemonSet); err != nil {
		t.Fatalf("%v\n%s", err, m[1].Raw)
	}
	if daemonSet.Kind != "DaemonSet" || len(daemonSet.Spec.Template.Spec.Volumes) != 1 || daemonSet.Spec.Template.Spec.Containers[0].ReadinessProbe.TCPSocket.Host != "127.0.0.1" {
		t.Fatalf("mirror workload:\n%s", m[1].Raw)
	}
}
