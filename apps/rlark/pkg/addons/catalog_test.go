package addons

import (
	"bytes"
	"io"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestRcloneManifestsUseSafeMountsAndCanarySelector(t *testing.T) {
	addon, ok := Registry.Get("csi-driver-rclone")
	if !ok {
		t.Fatal("csi-driver-rclone addon is not registered")
	}

	manifests, err := addon.Render(map[string]string{
		"nodeSelector":           "rlark.io/rclone-csi=enabled",
		"controllerNodeSelector": "kubernetes.io/hostname=worker-1",
	}, "rlark-system", "csi-driver-rclone", "test-uid")
	if err != nil {
		t.Fatalf("render addon: %v", err)
	}

	var foundController, foundNode, foundLeasePermissions bool
	for _, manifest := range manifests {
		decoder := yaml.NewDecoder(bytes.NewReader(manifest.Raw))
		for {
			var resource map[string]any
			if err := decoder.Decode(&resource); err != nil {
				if err == io.EOF {
					break
				}
				t.Fatalf("decode rendered manifest: %v", err)
			}
			if len(resource) == 0 {
				continue
			}

			switch resource["kind"] {
			case "Deployment":
				foundController = true
				spec := resource["spec"].(map[string]any)
				template := spec["template"].(map[string]any)
				podSpec := template["spec"].(map[string]any)
				selector := podSpec["nodeSelector"].(map[string]any)
				if selector["kubernetes.io/hostname"] != "worker-1" {
					t.Fatalf("unexpected controller selector: %v", selector)
				}
				containers := podSpec["containers"].([]any)
				provisioner := containers[1].(map[string]any)
				for _, arg := range provisioner["args"].([]any) {
					if arg == "--feature-gates=HonorPVReclaimPolicy=true" {
						t.Fatal("csi-provisioner v6.2.0 does not support HonorPVReclaimPolicy")
					}
				}
				for _, volume := range podSpec["volumes"].([]any) {
					if _, ok := volume.(map[string]any)["hostPath"]; ok {
						t.Fatalf("rclone controller must not mount host paths: %v", volume)
					}
				}
			case "ClusterRole":
				for _, rule := range resource["rules"].([]any) {
					rule := rule.(map[string]any)
					apiGroups := rule["apiGroups"].([]any)
					resources := rule["resources"].([]any)
					if len(apiGroups) == 1 && apiGroups[0] == "coordination.k8s.io" && len(resources) == 1 && resources[0] == "leases" {
						verbs := make(map[any]bool)
						for _, verb := range rule["verbs"].([]any) {
							verbs[verb] = true
						}
						for _, verb := range []string{"get", "list", "watch", "create", "update", "patch"} {
							if !verbs[verb] {
								t.Fatalf("missing lease permission %q: %v", verb, rule)
							}
						}
						foundLeasePermissions = true
					}
				}
			case "DaemonSet":
				foundNode = true
				spec := resource["spec"].(map[string]any)
				rollingUpdate := spec["updateStrategy"].(map[string]any)["rollingUpdate"].(map[string]any)
				if rollingUpdate["maxUnavailable"] != 1 {
					t.Fatalf("unexpected maxUnavailable: %v", rollingUpdate["maxUnavailable"])
				}
				template := spec["template"].(map[string]any)
				podSpec := template["spec"].(map[string]any)
				nodeSelector := podSpec["nodeSelector"].(map[string]any)
				if nodeSelector["rlark.io/rclone-csi"] != "enabled" {
					t.Fatalf("unexpected nodeSelector: %v", nodeSelector)
				}
				for _, volume := range podSpec["volumes"].([]any) {
					volume := volume.(map[string]any)
					hostPath, ok := volume["hostPath"].(map[string]any)
					if !ok {
						continue
					}
					if hostPath["path"] == "/var/lib/kubelet" {
						t.Fatal("rclone node must not mount the kubelet root directory")
					}
				}
			}
		}
	}

	if !foundController || !foundNode || !foundLeasePermissions {
		t.Fatalf("missing rendered resources: controller=%v node=%v leasePermissions=%v", foundController, foundNode, foundLeasePermissions)
	}
}

func TestRcloneSelectorsValidateAndDefault(t *testing.T) {
	addon, _ := Registry.Get("csi-driver-rclone")
	for _, name := range []string{"nodeSelector", "controllerNodeSelector"} {
		for _, value := range []string{"missing-equals", "a=b=c", "bad key=value", "valid/key=bad value"} {
			if _, err := addon.Render(map[string]string{name: value}, "rlark-system", "csi", "uid"); err == nil {
				t.Errorf("accepted invalid %s %q", name, value)
			}
		}
	}
	manifests, err := addon.Render(nil, "rlark-system", "csi", "uid")
	if err != nil {
		t.Fatal(err)
	}
	for _, manifest := range manifests {
		decoder := yaml.NewDecoder(bytes.NewReader(manifest.Raw))
		for {
			var resource map[string]any
			if err := decoder.Decode(&resource); err == io.EOF {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			if resource["kind"] != "Deployment" {
				continue
			}
			pod := resource["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			if _, ok := pod["nodeSelector"]; ok {
				t.Fatal("default controller unexpectedly restricts placement")
			}
		}
	}
}

func TestRclonePreservesExistingPodsWithOnDelete(t *testing.T) {
	addon, _ := Registry.Get("csi-driver-rclone")
	manifests, err := addon.Render(map[string]string{"nodeUpdateStrategy": "OnDelete", "nodeImagePullPolicy": "IfNotPresent"}, "rlark-system", "csi", "uid")
	if err != nil {
		t.Fatal(err)
	}
	for _, manifest := range manifests {
		decoder := yaml.NewDecoder(bytes.NewReader(manifest.Raw))
		for {
			var resource map[string]any
			if err := decoder.Decode(&resource); err == io.EOF {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			if resource["kind"] != "DaemonSet" {
				continue
			}
			spec := resource["spec"].(map[string]any)
			strategy := spec["updateStrategy"].(map[string]any)
			if strategy["type"] != "OnDelete" || strategy["rollingUpdate"] != nil {
				t.Fatalf("unexpected strategy: %v", strategy)
			}
			pod := spec["template"].(map[string]any)["spec"].(map[string]any)
			if pod["nodeSelector"] != nil {
				t.Fatal("default node placement restricted")
			}
			for _, c := range pod["containers"].([]any) {
				if c.(map[string]any)["imagePullPolicy"] != "IfNotPresent" {
					t.Fatalf("unexpected container: %v", c)
				}
			}
		}
	}
	for _, values := range []map[string]string{{"nodeUpdateStrategy": "invalid"}, {"nodeImagePullPolicy": "Never"}} {
		if _, err := addon.Render(values, "rlark-system", "csi", "uid"); err == nil {
			t.Fatal("invalid strategy accepted")
		}
	}
}

func TestRcloneNodeTpsLimitIsOptional(t *testing.T) {
	addon, _ := Registry.Get("csi-driver-rclone")
	for limit, want := range map[string]bool{"": false, "20": true} {
		manifests, err := addon.Render(map[string]string{"nodeTpsLimit": limit}, "cluster", "csi-driver-rclone", "uid")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, m := range manifests {
			found = found || bytes.Contains(m.Raw, []byte("name: RCLONE_TPSLIMIT\n              value: \"20\""))
		}
		if found != want {
			t.Fatalf("nodeTpsLimit=%q: RCLONE_TPSLIMIT rendered=%v", limit, found)
		}
	}
}

func TestRegistryTrustRendersHostsAndCA(t *testing.T) {
	addon, _ := Registry.Get("registry-trust")
	ca := "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----"
	manifests, err := addon.Render(map[string]string{"hosts": "10.0.0.1:5443,10.0.0.2:5443", "caCert": ca, "image": "registry/shell:1"}, "cluster", "registry-trust", "uid")
	if err != nil {
		t.Fatal(err)
	}
	var daemonSet struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Image string
						Env   []struct{ Name, Value string }
					}
					Volumes []struct {
						HostPath struct{ Path string } `yaml:"hostPath"`
					}
				}
			}
		}
	}
	if err := yaml.Unmarshal(manifests[0].Raw, &daemonSet); err != nil {
		t.Fatal(err)
	}
	pod := daemonSet.Spec.Template.Spec
	env := map[string]string{}
	for _, e := range pod.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["HOSTS"] != "10.0.0.1:5443,10.0.0.2:5443" || env["CA_CERT"] != ca+"\n" {
		t.Fatalf("env = %q", env)
	}
	if pod.Volumes[0].HostPath.Path != "/etc" || pod.Containers[0].Image != "registry/shell:1" {
		t.Fatalf("pod = %+v", pod)
	}
}
