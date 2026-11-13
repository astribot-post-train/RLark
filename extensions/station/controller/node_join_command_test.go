package controller

import (
	"strings"
	"testing"
)

func TestNodeJoinScriptValidatesAllInterpolatedValues(t *testing.T) {
	discovery := NodeJoinDiscovery{Endpoint: "10.11.14.128:16443", CAHash: "sha256:" + strings.Repeat("a", 64)}
	token := "123abc.0123456789abcdef"
	script, err := NodeJoinScript("worker-1", discovery, token)
	if err != nil || !strings.Contains(script, "kubeadm join") || !strings.Contains(script, "kubelet.conf") || !strings.Contains(script, "sysctl -n net.ipv4.ip_forward") || !strings.Contains(script, "--cri-socket 'unix:///run/containerd/containerd.sock'") {
		t.Fatalf("script: %v", err)
	}
	fleet := discovery
	fleet.CRISocket = "/run/containerd-fleet/containerd.sock"
	fleetScript, err := NodeJoinScript("worker-1", fleet, token)
	if err != nil || !strings.Contains(fleetScript, "--cri-socket 'unix:///run/containerd-fleet/containerd.sock'") {
		t.Fatalf("fleet socket script: %v", err)
	}
	for _, tc := range []struct {
		name      string
		discovery NodeJoinDiscovery
		token     string
	}{
		{"worker;touch", discovery, token},
		{"worker", NodeJoinDiscovery{Endpoint: "host;id:6443", CAHash: discovery.CAHash}, token},
		{"worker", NodeJoinDiscovery{Endpoint: discovery.Endpoint, CAHash: "sha256:" + strings.Repeat("z", 64)}, token},
		{"worker", discovery, "123abc.0123456789abcde;"},
		{"worker", NodeJoinDiscovery{Endpoint: discovery.Endpoint, CAHash: discovery.CAHash, CRISocket: "/tmp/unsafe.sock"}, token},
	} {
		if _, err := NodeJoinScript(tc.name, tc.discovery, tc.token); err == nil {
			t.Fatalf("accepted invalid input for %q", tc.name)
		}
	}
}

func TestNodeJoinAttemptScriptUsesPerOperationMarker(t *testing.T) {
	discovery := NodeJoinDiscovery{Endpoint: "10.11.14.128:16443", CAHash: "sha256:" + strings.Repeat("a", 64)}
	script, err := NodeJoinAttemptScript("01234567-89ab-cdef-0123-456789abcdef", "worker", discovery, "123abc.0123456789abcdef")
	if err != nil || !strings.Contains(script, "set -C") || !strings.Contains(script, "01234567-89ab-cdef-0123-456789abcdef.join") || strings.Index(script, "net.ipv4.ip_forward") > strings.Index(script, "set -C") {
		t.Fatalf("operation marker: %v", err)
	}
	for _, uid := range []string{"../../tmp/a", "01234567-89ab-cdef-0123-456789abcde;", "0123456789abcdef0123456789abcdef"} {
		if _, err := NodeJoinAttemptScript(uid, "worker", discovery, "123abc.0123456789abcdef"); err == nil {
			t.Fatalf("accepted invalid UID %q", uid)
		}
	}
}
