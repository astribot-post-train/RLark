package workflow

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	rlark "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestHistoricalTunnelRequiresNativeNodeAbsence(t *testing.T) {
	const document = `{"publicKey":"original-key"}`
	for _, tc := range []struct {
		name, body, publicKey string
		status                int
		want                  bool
	}{
		{"detached", `{"kind":"Status","reason":"NotFound","details":{"kind":"nodes","name":"box"}}`, "original-key", 404, true},
		{"replacement node", `{"metadata":{"uid":"replacement"}}`, "original-key", 200, false},
		{"proxy route missing", "404 page not found", "original-key", 404, false},
		{"another key", `{"kind":"Status","reason":"NotFound","details":{"kind":"nodes","name":"box"}}`, "different-key", 404, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := &rlark.NodeJoin{ObjectMeta: metav1.ObjectMeta{Name: "cluster-a.box", UID: "new-attempt"},
				Spec:   rlark.NodeJoinSpec{ClusterID: "cluster-a", NodeName: "box"},
				Status: rlark.NodeJoinStatus{HostKey: "SHA256:verified", Facts: rlark.NodeJoinFacts{PublicKey: tc.publicKey}}}
			box := &joinBox{answer: func(string) (string, error) { return fmt.Sprintf("%x\n%s\n", sha256.Sum256(nil), document), nil }}
			g := &Service{hooks: Hooks{ProxyKubeRequest: func(_ context.Context, method, agent, path string, _ []byte) (*http.Response, error) {
				if method != http.MethodGet || path != "api/v1/nodes/box" || agent != "cluster-a" {
					t.Fatalf("unexpected native lookup %s %s %s", method, agent, path)
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}}}
			proof, err := g.authorizeLegacyTunnel(t.Context(), box, j)
			if (err == nil) != tc.want {
				t.Fatalf("proof = %q, %v", proof, err)
			}
		})
	}
}

func TestHistoricalTunnelClaimRefusesChangedMarkerOrDocument(t *testing.T) {
	const oldDocument = `{"publicKey":"original-key"}`
	j := &rlark.NodeJoin{ObjectMeta: metav1.ObjectMeta{UID: "new-attempt"}}
	for _, tc := range []struct {
		marker, document string
		want             bool
	}{
		{"", oldDocument, true},
		{"previous-attempt", oldDocument, true},
		{"concurrent-owner", oldDocument, false},
		{"previous-attempt", `{"publicKey":"replacement-key"}`, false},
	} {
		t.Run(tc.marker+tc.document, func(t *testing.T) {
			dir := t.TempDir()
			markerPath := filepath.Join(dir, "tunnel.uid")
			documentPath := filepath.Join(dir, "bootstrap.json")
			before := tc.marker
			if !tc.want {
				before = "previous-attempt"
			}
			if tc.marker != "" {
				if err := os.WriteFile(markerPath, []byte(tc.marker+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(documentPath, []byte(tc.document+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			proof := fmt.Sprintf("%x/%x", sha256.Sum256([]byte(before)), sha256.Sum256([]byte(oldDocument)))
			script := strings.Replace(claimDetachedTunnel(j, proof), detachedJoinHost, "", 1)
			script = strings.NewReplacer(joinTunnelHostMarker, markerPath, legacyTunnelConfig, documentPath, "/var/lib/rlark", dir, "/etc/kubernetes/", dir+"/").Replace(script)
			out, err := exec.Command("sh", "-c", script).CombinedOutput()
			if (err == nil) != tc.want {
				t.Fatalf("claim expected %v: %s %v", tc.want, out, err)
			}
			got, _ := os.ReadFile(markerPath)
			if tc.want && string(got) != "new-attempt\n" || !tc.want && string(got) != tc.marker+"\n" {
				t.Fatalf("unexpected owner %q", got)
			}
		})
	}
}

func TestResidualWorkerRequiresMatchingJoinIdentityAndResets(t *testing.T) {
	for _, test := range []struct {
		identity string
		want     bool
	}{
		{"", false}, {"another-cluster/box", false}, {"cluster-a/other", false}, {"cluster-a/box", true},
	} {
		t.Run(test.identity, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "tunnel.uid")
			kubelet := filepath.Join(dir, "kubelet.conf")
			if err := os.WriteFile(kubelet, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if test.identity != "" {
				if err := os.WriteFile(marker+".identity", []byte(test.identity), 0600); err != nil {
					t.Fatal(err)
				}
			}
			fakeKubeadm := filepath.Join(dir, "kubeadm")
			if err := os.WriteFile(fakeKubeadm, []byte("#!/bin/sh\nrm -f "+kubelet+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			script := "set -eu\nmarker=" + marker + "\njoin_identity=cluster-a/box\nsystemctl() { :; }\n" + workerJoinIdentity + resetDetachedWorker
			script = strings.NewReplacer("/etc/kubernetes/", dir+"/", "/opt/rlark/bin/kubeadm", fakeKubeadm).Replace(script)
			out, err := exec.Command("sh", "-c", script).CombinedOutput()
			if (err == nil) != test.want {
				t.Fatalf("allowed = %v, output %s", err, out)
			}
			if _, err := os.Stat(kubelet); os.IsNotExist(err) != test.want {
				t.Fatalf("credentials: %v", err)
			}
		})
	}
}

func TestHistoricalWorkerRequiresOriginalMemberAndNode(t *testing.T) {
	const server = "https://member-api:6443"
	const config = `{"metadata":{"name":"cluster-info","namespace":"kube-public"},"data":{"kubeconfig":"apiVersion: v1\nkind: Config\nclusters:\n- name: member\n  cluster:\n    server: https://member-api:6443\n    certificate-authority-data: Y2E=\n"}}`
	correct := fmt.Sprintf("box/%x", sha256.Sum256([]byte(server+"\nca")))
	for _, test := range []struct {
		name, proof  string
		nativeStatus int
		want         bool
	}{
		{"original worker", correct, 404, true},
		{"same-name replacement", correct, 200, false},
		{"another cluster", fmt.Sprintf("box/%x", sha256.Sum256([]byte("https://replacement:6443\nca"))), 404, false},
		{"another node", "other/" + strings.Split(correct, "/")[1], 404, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			j := &rlark.NodeJoin{ObjectMeta: metav1.ObjectMeta{UID: "new-attempt"}, Spec: rlark.NodeJoinSpec{ClusterID: "cluster-a", NodeName: "box"}, Status: rlark.NodeJoinStatus{
				HostKey: "SHA256:pinned", Facts: rlark.NodeJoinFacts{PublicKey: "original-key"},
			}}
			box := &joinBox{answer: func(string) (string, error) {
				return fmt.Sprintf("worker=%s\n%x\n%s\n", test.proof, sha256.Sum256(nil), `{"publicKey":"original-key"}`), nil
			}}
			g := &Service{hooks: Hooks{ProxyKubeRequest: func(_ context.Context, method, agent, path string, _ []byte) (*http.Response, error) {
				if method != http.MethodGet || agent != "cluster-a" {
					t.Fatalf("unexpected native request: %s %s", method, agent)
				}
				switch path {
				case "api/v1/namespaces/kube-public/configmaps/cluster-info":
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(config))}, nil
				case "api/v1/nodes/box":
					return &http.Response{StatusCode: test.nativeStatus, Body: io.NopCloser(strings.NewReader(`{"reason":"NotFound","details":{"kind":"nodes","name":"box"}}`))}, nil
				default:
					return nil, fmt.Errorf("unexpected node lookup %s", path)
				}
			}}}
			proof, err := g.authorizeLegacyTunnel(t.Context(), box, j)
			if (err == nil) != test.want || (test.want && !strings.Contains(proof, correct)) {
				t.Fatalf("proof = %q, %v", proof, err)
			}
		})
	}
}
