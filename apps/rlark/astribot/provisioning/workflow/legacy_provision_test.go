package workflow

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	rlark "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/provisioning/ownership"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMigrateDeletedLegacyProvisionLeavesHistoryUntilOwned(t *testing.T) {
	const name = "rlark-baidu-gpu"
	const route = "/cluster-provisions/" + name
	network := provisionTestNetwork()
	network.SetUID("original-network")
	network.SetResourceVersion("2")
	spec := network.Object["spec"].(map[string]any)
	spec["sites"] = append(spec["sites"].([]any), map[string]any{"name": "baidu", "clusters": []any{name, "another-cluster"}})
	spec["names"] = append(spec["names"].([]any), map[string]any{"name": "cp." + name + ".rlark.internal", "address": "10.254.240.6"})
	spec["nodes"] = append(spec["nodes"].([]any), map[string]any{"clusterID": name, "nodeName": "gpu-1", "publicKey": "original-key", "address": "10.254.240.6"})
	provision := &rlark.ClusterProvision{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: "original-provision", ResourceVersion: "10"},
		Spec:       rlark.ClusterProvisionSpec{Site: "baidu"},
		Status:     rlark.ClusterProvisionStatus{Facts: rlark.ClusterProvisionFacts{NodeName: "gpu-1", TunnelAddress: "10.254.240.6", PublicKey: "original-key"}},
	}
	g := provisionGateway(t, network, provision)
	ref := g.provisionCredentials(name)["station-read"]
	credentials := g.rawClient.CoreV1().Secrets(ref.namespace)
	if _, err := credentials.Create(t.Context(), &core.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: ref.name, Namespace: ref.namespace, UID: "original-secret", ResourceVersion: "3"},
		Data:       map[string][]byte{"test": []byte("untouched")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if code, body := provisionCall(t, g, http.MethodPost, route+"/clear", name, `{"machine_gone":true}`); code != http.StatusConflict {
		t.Fatalf("cleared before migration: %d %v", code, body)
	}
	for _, attempt := range []struct {
		uid  string
		want int
	}{
		{"same-name-replacement", http.StatusConflict}, {"original-secret", http.StatusOK}, {"original-secret", http.StatusOK},
	} {
		body := `{"provision_uid":"original-provision","evidence":"verified change record","site_network":{"uid":"original-network","resource_version":"2"},"credentials":{"station-read":{"uid":"` + attempt.uid + `","resource_version":"3"}}}`
		if code, response := provisionCall(t, g, http.MethodPost, route+"/migrate-ownership", name, body); code != attempt.want {
			t.Fatalf("migration %s = %d %v", attempt.uid, code, response)
		}
	}
	secret, err := credentials.Get(t.Context(), ref.name, metav1.GetOptions{})
	if err != nil || !ownership.CredentialMatches(secret, provision.UID) || string(secret.Data["test"]) != "untouched" {
		t.Fatalf("migration changed data or failed: %v", err)
	}
	if code, response := provisionCall(t, g, http.MethodPost, route+"/clear", name, `{"machine_gone":true}`); code != http.StatusOK {
		t.Fatalf("clear after migration = %d %v", code, response)
	}
	if _, err := credentials.Get(t.Context(), ref.name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("old credential left: %v", err)
	}
	live, err := g.provisionNetwork(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range live.Entries("spec", "sites") {
		if entry["name"] == "baidu" {
			clusters := entry["clusters"].([]any)
			if len(clusters) == 1 && clusters[0] == "another-cluster" {
				return
			}
		}
	}
	t.Fatal("cleanup removed another member of the same site")
}

func TestHistoricalControlPlaneVerifiesNativeUIDBeforeClaim(t *testing.T) {
	for _, test := range []struct {
		name, observed, previous string
		want                     bool
	}{
		{"original", "native-original", "", true},
		{"native replacement", "native-replacement", "", false},
		{"another provision", "native-original", "different-provision", false},
		{"repeat after cleanup", "", "cleared-original-provision", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "provision")
			if test.previous != "" {
				if err := os.WriteFile(marker, []byte(test.previous), 0600); err != nil {
					t.Fatal(err)
				}
			}
			script := strings.Replace(claimLegacyProvisionHost("original-provision", "native-original"), readHostClusterUID,
				"\nnative_cluster_uid() { printf '%s' '"+test.observed+"'; }\n", 1)
			script = strings.NewReplacer(provisionHostMarker, marker, "/var/lib/rlark", dir).Replace(script)
			out, err := exec.Command("sh", "-c", script).CombinedOutput()
			if (err == nil) != test.want {
				t.Fatalf("claimed = %v, output %s", err, out)
			}
			if !test.want && test.previous == "" {
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatalf("replacement got a marker: %v", err)
				}
			}
		})
	}
}
