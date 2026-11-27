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
			script = strings.NewReplacer(joinTunnelHostMarker, markerPath, legacyTunnelConfig, documentPath, "/var/lib/rlark", dir).Replace(script)
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
