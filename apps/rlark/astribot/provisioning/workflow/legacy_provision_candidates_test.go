package workflow

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	rlark "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/provisioning"
	"github.com/rlinf/rlark/apps/rlark/astribot/provisioning/ownership"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMigrationPreviewIncludesOnlyMetadataAndRejectsStaleConfirmation(t *testing.T) {
	const name = "rlark-baidu-gpu"
	route := "/cluster-provisions/" + name
	p := &rlark.ClusterProvision{ObjectMeta: metav1.ObjectMeta{Name: name, UID: "provision-original", ResourceVersion: "4"}}
	g := provisionGateway(t, provisionTestNetwork(), p)
	ref := g.provisionCredentials(name)["station-read"]
	secrets := g.rawClient.CoreV1().Secrets(ref.namespace)
	original := &core.Secret{ObjectMeta: metav1.ObjectMeta{Name: ref.name, Namespace: ref.namespace, UID: "secret-original", ResourceVersion: "8"}, Data: map[string][]byte{"private": []byte("do-not-expose")}}
	if _, err := secrets.Create(t.Context(), original, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	other := g.provisionCredentials(name)["release"]
	if _, err := g.rawClient.CoreV1().Secrets(other.namespace).Create(t.Context(), &core.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: other.name, Namespace: other.namespace, UID: "owned-by-other", ResourceVersion: "1", Annotations: map[string]string{ownership.ProvisionUID: "another-provision"},
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	code, response := provisionCall(t, g, http.MethodGet, route+"/migration-candidates", name, "")
	if code != http.StatusOK {
		t.Fatalf("preview: %d %v", code, response)
	}
	encoded, err := json.Marshal(response["data"])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "do-not-expose") || strings.Contains(string(encoded), `"private"`) || strings.Contains(string(encoded), "owned-by-other") {
		t.Fatal("preview leaked a credential value or a blocked identity")
	}
	var preview migrationCandidates
	if err := json.Unmarshal(encoded, &preview); err != nil {
		t.Fatal(err)
	}
	var snapshot *reviewedObject
	for _, item := range preview.Candidates {
		switch item.Key {
		case "station-read":
			if item.State != migrationReview {
				t.Fatalf("station credential: %+v", item)
			}
			snapshot = item.Identity
		case "release":
			if item.State != migrationBlocked || item.Identity != nil {
				t.Fatalf("ownership conflict: %+v", item)
			}
		}
	}
	if snapshot == nil {
		t.Fatal("missing resource version snapshot")
	}
	payload, err := json.Marshal(provisionMigration{ProvisionUID: preview.ProvisionUID, ProvisionResourceVersion: preview.ProvisionResourceVersion,
		Evidence: "CHG-123", Credentials: map[string]reviewedObject{"station-read": *snapshot}})
	if err != nil {
		t.Fatal(err)
	}
	changed := original.DeepCopy()
	changed.ResourceVersion = "9"
	if _, err := secrets.Update(t.Context(), changed, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if code, response := provisionCall(t, g, http.MethodPost, route+"/migrate-ownership", name, string(payload)); code != http.StatusConflict {
		t.Fatalf("stale confirmation: %d %v", code, response)
	}
	current, err := secrets.Get(t.Context(), ref.name, metav1.GetOptions{})
	if err != nil || current.Annotations[ownership.ProvisionUID] != "" {
		t.Fatalf("stale confirmation wrote ownership: %v", err)
	}
	snapshot.ResourceVersion = "9"
	payload, err = json.Marshal(provisionMigration{ProvisionUID: preview.ProvisionUID, ProvisionResourceVersion: preview.ProvisionResourceVersion,
		Evidence: "CHG-123", Credentials: map[string]reviewedObject{"station-read": *snapshot}})
	if err != nil {
		t.Fatal(err)
	}
	if code, response := provisionCall(t, g, http.MethodPost, route+"/migrate-ownership", name, string(payload)); code != http.StatusOK {
		t.Fatalf("fresh confirmation: %d %v", code, response)
	}
}

func TestMigrationHostPreviewIsReadOnlyAndUsesPinnedOriginalHost(t *testing.T) {
	const name = "rlark-baidu-gpu"
	p := &rlark.ClusterProvision{ObjectMeta: metav1.ObjectMeta{Name: name, UID: "provision-original", ResourceVersion: "4"},
		Spec:   rlark.ClusterProvisionSpec{Machine: rlark.ClusterProvisionMachine{Address: "192.0.2.4"}},
		Status: rlark.ClusterProvisionStatus{HostKey: "SHA256:original"},
	}
	g := provisionGateway(t, provisionTestNetwork(), p)
	machine := &fakeMachine{answers: map[string]string{"get namespace kube-system": "native-original"}}
	g.provisionDial = func(_ context.Context, machineSpec rlark.ClusterProvisionMachine, password []byte, fingerprint string) (provisioning.Machine, string, error) {
		if machineSpec.Address != "192.0.2.4" || fingerprint != "SHA256:original" || string(password) != "fixture" {
			t.Fatal("unverified host")
		}
		return machine, fingerprint, nil
	}
	route := "/cluster-provisions/" + name + "/migration-candidates/host"
	for _, item := range []struct {
		version string
		code    int
	}{{"3", 409}, {"4", 200}} {
		code, out := provisionCall(t, g, http.MethodPost, route, name, `{"provision_uid":"provision-original","provision_resource_version":"`+item.version+`","password":"fixture"}`)
		if code != item.code {
			t.Fatalf("host preview: %d %v", code, out)
		}
		if code == 200 && out["data"].(map[string]any)["uid"] != "native-original" {
			t.Fatalf("unexpected candidate: %v", out)
		}
	}
	if !machine.closed || len(machine.ran) != 1 || !strings.Contains(machine.ran[0], "get namespace kube-system") || strings.Contains(machine.ran[0], "reset") {
		t.Fatal("host preview did not perform exactly one read")
	}
	current, err := g.kubeClient.RlinfV1alpha1().ClusterProvisions().Get(t.Context(), name, metav1.GetOptions{})
	if err != nil || current.Annotations[legacyNativeClusterUID] != "" {
		t.Fatalf("preview registered ownership: %v", err)
	}
}
