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
	values := map[string]string{"image": "registry@sha256:x", "relayImage": "rlark@sha256:y", "s3Bucket": "b", "s3Region": "r", "s3Endpoint": "https://s3", "s3RootDirectory": "/root"}

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
	if err := yaml.UnmarshalStrict(m[2].Raw, &deployment); err != nil {
		t.Fatalf("%v\n%s", err, m[2].Raw)
	}
	spec := deployment.Spec.Template.Spec
	if deployment.Kind != "Deployment" || spec.NodeSelector["rlark.io/site-registry"] != "true" || len(spec.Volumes) != 2 || spec.Containers[0].ReadinessProbe.TCPSocket.Port.IntValue() != 5443 {
		t.Fatalf("registry workload:\n%s", m[2].Raw)
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
	if err := yaml.UnmarshalStrict(m[2].Raw, &daemonSet); err != nil {
		t.Fatalf("%v\n%s", err, m[1].Raw)
	}
	if daemonSet.Kind != "DaemonSet" || len(daemonSet.Spec.Template.Spec.Volumes) != 1 || daemonSet.Spec.Template.Spec.Containers[0].ReadinessProbe.TCPSocket.Host != "127.0.0.1" {
		t.Fatalf("mirror workload:\n%s", m[2].Raw)
	}

	cache := map[string]string{"role": "cache", "image": "registry@sha256:x", "relayImage": "rlark@sha256:y", "prewarm": "registry.rlark.internal:5443/fleet/robot@sha256:z", "listen": "0.0.0.0:5443", "upstream": "https://origin.rlark.internal:5443",
		"caCert": "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----", "cacheDir": "/var/lib/rlark/site-cache", "ttl": "336h"}
	m, err = a.Render(cache, "ns", "site-cache", "u")
	if err != nil {
		t.Fatal(err)
	}
	config = corev1.ConfigMap{}
	if err := yaml.UnmarshalStrict(m[0].Raw, &config); err != nil {
		t.Fatalf("%v\n%s", err, m[0].Raw)
	}
	body := config.Data["config.yml"]
	if !strings.Contains(body, "remoteurl: https://origin.rlark.internal:5443") || !strings.Contains(body, "ttl: 336h") || !strings.Contains(body, "rootdirectory: /var/lib/registry") ||
		strings.Contains(body, "s3:") || !strings.Contains(body, "htpasswd") || !strings.Contains(body, "command: /etc/distribution/credential-helper") ||
		!strings.Contains(config.Data["upstream-ca.crt"], "AAAA") || !strings.HasPrefix(config.Data["credential-helper"], "#!/bin/sh") || !strings.Contains(config.Data["prewarm.sh"], "prewarm done") {
		t.Fatalf("cache config:\n%s", m[0].Raw)
	}
	var prewarm appsv1.Deployment
	if err := yaml.UnmarshalStrict(m[1].Raw, &prewarm); err != nil {
		t.Fatalf("%v\n%s", err, m[1].Raw)
	}
	if prewarm.Name != "site-cache-prewarm" || prewarm.Spec.Template.Spec.InitContainers[0].Env[1].Value != "registry.rlark.internal:5443/fleet/robot@sha256:z" ||
		prewarm.Spec.Template.Spec.InitContainers[0].Env[0].Value != "https://registry.rlark.internal:5443" {
		t.Fatalf("prewarm:\n%s", m[1].Raw)
	}
	deployment = appsv1.Deployment{}
	if err := yaml.UnmarshalStrict(m[2].Raw, &deployment); err != nil {
		t.Fatalf("%v\n%s", err, m[1].Raw)
	}
	spec = deployment.Spec.Template.Spec
	env := map[string]bool{}
	for _, e := range spec.Containers[0].Env {
		env[e.Name] = true
	}
	if deployment.Kind != "Deployment" || spec.NodeSelector["rlark.io/site-registry"] != "true" || len(spec.Volumes) != 3 ||
		spec.Volumes[1].HostPath == nil || spec.Volumes[1].HostPath.Path != "/var/lib/rlark/site-cache" ||
		env["REGISTRY_PROXY_USERNAME"] || !env["HTTPS_PROXY"] || !env["SSL_CERT_FILE"] || !env["GODEBUG"] || env["REGISTRY_STORAGE_S3_ACCESSKEY"] ||
		len(spec.Containers) != 2 || spec.Containers[1].Image != "rlark@sha256:y" || len(spec.Volumes[2].Secret.Items) != 5 {
		t.Fatalf("cache workload:\n%s", m[2].Raw)
	}
}

func TestRegistryTrustRendersOneDaemonSetPerNodeClass(t *testing.T) {
	a, _ := Registry.Get("registry-trust")
	m, err := a.Render(map[string]string{"hosts": "registry.rlark.internal:5443", "caCert": "x", "image": "busybox", "kubectlImage": "kubectl"}, "ns", "registry-trust", "u")
	if err != nil {
		t.Fatal(err)
	}
	var daemonSets []appsv1.DaemonSet
	for _, manifest := range m {
		for _, doc := range strings.Split(string(manifest.Raw), "\n---\n") {
			var ds appsv1.DaemonSet
			if err := yaml.Unmarshal([]byte(doc), &ds); err == nil && ds.Kind == "DaemonSet" {
				daemonSets = append(daemonSets, ds)
			}
		}
	}
	if len(daemonSets) != 2 {
		t.Fatalf("got %d DaemonSets, want 2", len(daemonSets))
	}
	for i, want := range []struct{ name, operator, robot string }{{"registry-trust", "NotIn", "false"}, {"registry-trust-robot", "In", "true"}} {
		ds := daemonSets[i]
		term := ds.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0]
		if ds.Name != want.name || string(term.Operator) != want.operator || ds.Spec.Selector.MatchLabels["app"] != want.name || ds.Spec.Template.Spec.InitContainers[0].Env[0].Value != want.robot {
			t.Fatalf("DaemonSet %d: %s %s %v %s", i, ds.Name, term.Operator, ds.Spec.Selector.MatchLabels, ds.Spec.Template.Spec.InitContainers[0].Env[0].Value)
		}
	}
}
