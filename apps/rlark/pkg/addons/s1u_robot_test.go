package addons

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

func renderS1URobot(t *testing.T, values map[string]string) appsv1.DaemonSet {
	t.Helper()
	a, ok := Registry.Get("s1u-robot")
	if !ok {
		t.Fatal("s1u-robot not in catalog")
	}
	manifests, err := a.Render(values, "cluster", "s1u-robot", "uid")
	if err != nil {
		t.Fatal(err)
	}
	for _, manifest := range manifests {
		for _, document := range strings.Split(string(manifest.Raw), "\n---\n") {
			if !strings.Contains(document, "kind: DaemonSet") {
				continue
			}
			var daemonSet appsv1.DaemonSet
			if err := yaml.UnmarshalStrict([]byte(document), &daemonSet); err != nil {
				t.Fatal(err)
			}
			return daemonSet
		}
	}
	t.Fatal("no DaemonSet rendered")
	return appsv1.DaemonSet{}
}

func TestS1URobotRendersItsContainersOnS1UNodesAndStaysThroughWhatKeepsOthersOff(t *testing.T) {
	daemonSet := renderS1URobot(t, map[string]string{"image": "registry/rlark/s1u-robot@sha256:1", "imagePullSecret": "ir-0"})
	pod := daemonSet.Spec.Template.Spec
	if pod.NodeSelector["rlark.io/robot-model"] != "s1u" || pod.PriorityClassName != "system-node-critical" || len(pod.InitContainers) != 0 {
		t.Fatalf("pod = %+v", pod)
	}
	if len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != "ir-0" {
		t.Fatalf("pull secrets = %+v", pod.ImagePullSecrets)
	}
	names := []string{}
	for _, container := range pod.Containers {
		names = append(names, container.Name)
		if container.Image != "registry/rlark/s1u-robot@sha256:1" {
			t.Fatalf("%s image = %s", container.Name, container.Image)
		}
	}
	if strings.Join(names, ",") != "tuner,orin,power,clock" {
		t.Fatalf("containers = %v", names)
	}
	env := map[string]string{}
	for _, variable := range pod.Containers[0].Env {
		env[variable.Name] = variable.Value
	}
	// The defaults of the parameters reach the tuner.
	if env["S1U_IMAGE"] != "registry/rlark/s1u-robot@sha256:1" || env["S1U_NODE_REBOOT"] != "idle" ||
		env["S1U_NODE_ROLLOUT"] != "staged" || env["S1U_NODE_ROLLOUT_SECONDS"] != "600" || env["S1U_NODE_WANTED"] != "installed" {
		t.Fatalf("tuner env = %v", env)
	}
	// Every toleration names its taint: none for any taint, none for a node declared out of service.
	for _, toleration := range pod.Tolerations {
		if toleration.Key == "" || toleration.Key == "node.kubernetes.io/out-of-service" || toleration.TolerationSeconds != nil {
			t.Fatalf("toleration %+v", toleration)
		}
	}
	power := map[string]string{}
	for _, variable := range pod.Containers[2].Env {
		power[variable.Name] = variable.Value
	}
	// The driver starts by itself after a power-on, the motors are powered by a person: the defaults.
	if power["S1U_AUTO_DRIVER"] != "1" || power["S1U_AUTO_POWER"] != "0" || power["S1U_SERVICE_SOCKET"] != "/services/s1u-power.sock" {
		t.Fatalf("power env = %v", power)
	}
	if pod.Volumes[0].Secret.SecretName != "s1u-orin-access" || pod.Volumes[0].Secret.Optional == nil || !*pod.Volumes[0].Secret.Optional {
		t.Fatalf("orin access volume = %+v", pod.Volumes[0])
	}
}

func TestS1URobotCopiesEachPayloadForTheOrinAndRetiresDeclaredUnits(t *testing.T) {
	daemonSet := renderS1URobot(t, map[string]string{
		"image":            "registry/rlark/s1u-robot@sha256:1",
		"orinPayloads":     "quest-bridge=registry/fleet/quest-bridge@sha256:2,leds=registry/x/leds:3",
		"orinRetiredUnits": "fleet-orin-uplink",
	})
	pod := daemonSet.Spec.Template.Spec
	if len(pod.InitContainers) != 2 {
		t.Fatalf("init containers = %+v", pod.InitContainers)
	}
	first := pod.InitContainers[0]
	if first.Image != "registry/fleet/quest-bridge@sha256:2" || !strings.Contains(first.Command[2], "cp -a /payload/. /payloads/quest-bridge/") {
		t.Fatalf("first payload = %+v", first)
	}
	if pod.InitContainers[1].Image != "registry/x/leds:3" || !strings.Contains(pod.InitContainers[1].Command[2], "/payloads/leds/") {
		t.Fatalf("second payload = %+v", pod.InitContainers[1])
	}
	env := map[string]string{}
	for _, variable := range pod.Containers[1].Env {
		env[variable.Name] = variable.Value
	}
	if env["S1U_ORIN_RETIRED_UNITS"] != "fleet-orin-uplink" {
		t.Fatalf("orin env = %v", env)
	}
}
