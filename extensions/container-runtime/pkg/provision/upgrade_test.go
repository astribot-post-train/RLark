package provision

import (
	"context"
	"errors"
	"strings"
	"testing"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/companion"
	"github.com/google/uuid"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func provisionUpgradeFixture(t *testing.T) (Service, UpgradeRequest) {
	t.Helper()
	s, node := registered(t)
	ctx := context.Background()
	var cc ext.ContainerCluster
	must(t, s.Client.Get(ctx, client.ObjectKey{Namespace: node.Namespace, Name: node.Spec.Cluster.Name}, &cc))
	cc.Spec.TargetAgentRelease = "next"
	must(t, s.Client.Update(ctx, &cc))
	release := &ext.AgentRelease{ObjectMeta: metav1.ObjectMeta{Name: "next", Namespace: node.Namespace, Labels: map[string]string{tenant.IDLabel: tenant.IDOf(node)}}, Spec: ext.AgentReleaseSpec{Version: "next", OS: "linux", Architecture: "amd64", Protocol: "v1", SHA256: strings.Repeat("0", 64), ArtifactPath: "agent"}, Status: ext.AgentReleaseStatus{Available: true, ObservedGeneration: 1}}
	must(t, s.Client.Create(ctx, release))
	return s, UpgradeRequest{OperationID: uuid.NewString(), Node: nodeID(node), Release: ref(release), SHA256: release.Spec.SHA256, FromVersion: "old"}
}

func TestCreateUpgradeIsRetainedIntentNotInstallation(t *testing.T) {
	s, req := provisionUpgradeFixture(t)
	ctx := context.Background()
	op, err := s.CreateUpgrade(ctx, req)
	must(t, err)
	if op.Name != companion.UpgradeName(op.Spec.Cluster.UID) || op.Spec.Release != req.Release || op.Spec.FromVersion != req.FromVersion || op.Status.Phase != "" || len(op.Finalizers) != 0 {
		t.Fatalf("unexpected operation: %#v", op)
	}
	again, err := s.CreateUpgrade(ctx, req)
	must(t, err)
	if again.UID != op.UID {
		t.Fatal("retry created a different operation")
	}
	var locks ext.ContainerAllocationList
	must(t, s.Client.List(ctx, &locks))
	if len(locks.Items) != 0 {
		t.Fatal("creation reserved or installed without controller")
	}
	changed := req
	changed.OperationID = uuid.NewString()
	if _, err := s.CreateUpgrade(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("second operation took active cluster slot: %v", err)
	}
	changed = req
	changed.FromVersion = "different"
	if _, err := s.CreateUpgrade(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("retry changed immutable intent: %v", err)
	}
}

func TestCreateUpgradeRecoversCommittedResponseLoss(t *testing.T) {
	s, req := provisionUpgradeFixture(t)
	ctx := context.Background()
	base := s.Client
	lost := false
	s.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if err := c.Create(ctx, obj, opts...); err != nil {
			return err
		}
		if _, ok := obj.(*ext.AgentUpgrade); ok && !lost {
			lost = true
			return errors.New("committed response lost")
		}
		return nil
	}})
	first, err := s.CreateUpgrade(ctx, req)
	must(t, err)
	again, err := s.CreateUpgrade(ctx, req)
	must(t, err)
	if first.UID != again.UID {
		t.Fatal("lost create response duplicated operation")
	}
}

func TestCreateUpgradeRejectsIdentityAndPublicationConflicts(t *testing.T) {
	for _, mode := range []string{"bad operation", "node UID", "tenant", "credential", "release UID", "release digest", "release namespace", "target", "foreign release", "unavailable release", "foreign operation"} {
		t.Run(mode, func(t *testing.T) {
			s, req := provisionUpgradeFixture(t)
			ctx := context.Background()
			switch mode {
			case "bad operation":
				req.OperationID = "bad"
			case "node UID":
				req.Node.UID = "old"
			case "tenant":
				req.Node.TenantID = "other"
			case "credential":
				req.Node.CredentialID = "old"
			case "release UID":
				req.Release.UID = "old"
			case "release digest":
				req.SHA256 = strings.Repeat("1", 64)
			case "release namespace":
				req.Release.Namespace = "other"
			case "target":
				var cc ext.ContainerCluster
				must(t, s.Client.Get(ctx, client.ObjectKey{Namespace: req.Node.Namespace, Name: "cloud"}, &cc))
				cc.Spec.TargetAgentRelease = "other"
				must(t, s.Client.Update(ctx, &cc))
			case "foreign release", "unavailable release":
				var release ext.AgentRelease
				must(t, s.Client.Get(ctx, client.ObjectKey{Namespace: req.Release.Namespace, Name: req.Release.Name}, &release))
				if mode == "foreign release" {
					release.Labels[tenant.IDLabel] = "other"
				} else {
					release.Status.Available = false
				}
				must(t, s.Client.Update(ctx, &release))
			case "foreign operation":
				op, err := s.CreateUpgrade(ctx, req)
				must(t, err)
				delete(op.Annotations, OperationAnnotation)
				must(t, s.Client.Update(ctx, op))
			}
			if _, err := s.CreateUpgrade(ctx, req); err == nil {
				t.Fatal("conflicting upgrade was accepted")
			}
		})
	}
}

func TestCreateUpgradeRechecksCredentialAfterWrite(t *testing.T) {
	s, req := provisionUpgradeFixture(t)
	ctx := context.Background()
	base := s.Client
	s.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if err := c.Create(ctx, obj, opts...); err != nil {
			return err
		}
		var node ext.ContainerNode
		if err := c.Get(ctx, client.ObjectKey{Namespace: req.Node.Namespace, Name: req.Node.Name}, &node); err != nil {
			return err
		}
		node.Annotations[ext.AgentCredentialAnnotation] = "rotated"
		return c.Update(ctx, &node)
	}})
	if _, err := s.CreateUpgrade(ctx, req); !errors.Is(err, ErrConflict) {
		t.Fatalf("credential change during creation accepted: %v", err)
	}
	var operations ext.AgentUpgradeList
	must(t, base.List(ctx, &operations))
	if len(operations.Items) != 1 {
		t.Fatal("uncertain creation was erased or duplicated")
	}
}
