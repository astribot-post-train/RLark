package provision

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/core/api/v1alpha1"
	"github.com/google/uuid"
	tenant "github.com/rlinf/rlark/api/metadata"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func testEnrollment(t *testing.T) (Service, EnrollmentRequest) {
	t.Helper()
	s, req := fixture(t)
	cc, err := s.CreateCluster(context.Background(), req)
	must(t, err)
	release := &ext.AgentRelease{ObjectMeta: metav1.ObjectMeta{Name: "v1", Namespace: cc.Namespace, Labels: map[string]string{tenant.IDLabel: req.TenantID}}, Spec: ext.AgentReleaseSpec{Version: "v1", OS: "linux", Architecture: "amd64", Protocol: "v1", ArtifactPath: "release/v1", SHA256: strings.Repeat("a", 64)}}
	must(t, s.Client.Create(context.Background(), release))
	release.Status.Available = true
	release.Status.ObservedGeneration = release.Generation
	// This fixture does not configure a release status subresource.
	must(t, s.Client.Update(context.Background(), release))
	return s, EnrollmentRequest{OperationID: uuid.NewString(), Namespace: cc.Namespace, Cluster: cc.Name, ClusterUID: string(cc.UID), TenantID: req.TenantID, Nodes: []ext.EnrollmentNode{{Name: "a", Role: ext.NodeRoleGateway, Address: "203.0.113.2"}, {Name: "b", Role: ext.NodeRoleWorker, Address: "10.0.0.2"}}}
}
func TestEnrollmentPinsLatestAndRetainsOperationOnRetry(t *testing.T) {
	s, req := testEnrollment(t)
	before := append([]ext.EnrollmentNode(nil), req.Nodes...)
	op, err := s.CreateEnrollment(context.Background(), req)
	must(t, err)
	if !reflect.DeepEqual(req.Nodes, before) || op.Spec.Nodes[0].Port != 22 {
		t.Fatal("mutated request or lost port default")
	}
	var old ext.AgentRelease
	must(t, s.Client.Get(context.Background(), client.ObjectKey{Namespace: req.Namespace, Name: "v1"}, &old))
	newer := old.DeepCopy()
	newer.Name = "v2"
	newer.UID = ""
	newer.ResourceVersion = ""
	newer.Spec.Version = "v2"
	newer.CreationTimestamp = metav1.Now()
	must(t, s.Client.Create(context.Background(), newer))
	again, err := s.CreateEnrollment(context.Background(), req)
	must(t, err)
	if again.Spec.Release != op.Spec.Release || again.UID != op.UID {
		t.Fatal("retry changed pinned release")
	}
	req.Nodes[1].Address = "10.0.0.3"
	if _, err = s.CreateEnrollment(context.Background(), req); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed operation accepted: %v", err)
	}
}
func TestEnrollmentTopologyValidation(t *testing.T) {
	_, base := testEnrollment(t)
	for _, change := range []func([]ext.EnrollmentNode){func(n []ext.EnrollmentNode) { n[1].Role = ext.NodeRoleGateway }, func(n []ext.EnrollmentNode) { n[1].Role = "unknown" }, func(n []ext.EnrollmentNode) { n[1].Address = "203.0.113.3" }, func(n []ext.EnrollmentNode) { n[1].Address = "127.0.0.1" }, func(n []ext.EnrollmentNode) { n[1].Address = "169.254.1.2" }, func(n []ext.EnrollmentNode) { n[1].Name = n[0].Name }, func(n []ext.EnrollmentNode) { n[1].Port = -1 }} {
		nodes := append([]ext.EnrollmentNode(nil), base.Nodes...)
		for i := range nodes {
			nodes[i].Port = 22
		}
		change(nodes)
		if ValidateEnrollmentNodes(nodes) == nil {
			t.Fatalf("accepted invalid topology: %#v", nodes)
		}
	}
}
