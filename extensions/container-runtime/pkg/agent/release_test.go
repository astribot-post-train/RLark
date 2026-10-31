package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/artifact"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type releaseFixture struct {
	reporter Reporter
	identity Identity
	release  *ext.AgentRelease
	root     string
	data     []byte
}

func newReleaseFixture(t *testing.T) releaseFixture {
	t.Helper()
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("Linux ELF artifact")
	}
	r, id, node := observerFixture(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "agent"), data, 0700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	release := &ext.AgentRelease{ObjectMeta: metav1.ObjectMeta{Name: "v1", Namespace: node.Namespace, UID: "release-uid", Generation: 1, Labels: map[string]string{tenant.IDLabel: tenant.DefaultID}}, Spec: ext.AgentReleaseSpec{Version: "v1", OS: "linux", Architecture: runtime.GOARCH, SHA256: hex.EncodeToString(digest[:]), ArtifactPath: "agent", Protocol: "v1"}, Status: ext.AgentReleaseStatus{ObservedGeneration: 1, Available: true}}
	var cluster ext.ContainerCluster
	if err := r.Client.Get(context.Background(), client.ObjectKey{Namespace: node.Namespace, Name: node.Spec.Cluster.Name}, &cluster); err != nil {
		t.Fatal(err)
	}
	cluster.Spec.TargetAgentRelease = release.Name
	if err := r.Client.Update(context.Background(), &cluster); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Create(context.Background(), release); err != nil {
		t.Fatal(err)
	}
	return releaseFixture{reporter: r, identity: id, release: release, root: root, data: data}
}

type releaseStoreFunc func(context.Context, ext.AgentReleaseSpec) ([]byte, error)

func (f releaseStoreFunc) Load(ctx context.Context, spec ext.AgentReleaseSpec) ([]byte, error) {
	return f(ctx, spec)
}

func TestReleaseRejectsChangedTargetAfterArtifactRead(t *testing.T) {
	f := newReleaseFixture(t)
	store := releaseStoreFunc(func(ctx context.Context, _ ext.AgentReleaseSpec) ([]byte, error) {
		var cc ext.ContainerCluster
		if err := f.reporter.Client.Get(ctx, client.ObjectKey{Namespace: f.identity.Namespace, Name: "cloud"}, &cc); err != nil {
			return nil, err
		}
		cc.Spec.TargetAgentRelease = "missing-replacement"
		return f.data, f.reporter.Client.Update(ctx, &cc)
	})
	_, data, err := f.reporter.Release(context.Background(), f.identity, ReleaseRequest{Action: "manifest", OS: "linux", Architecture: runtime.GOARCH}, store)
	if !errors.Is(err, ErrReleaseUnavailable) || data != nil {
		t.Fatalf("changed target published bytes=%d err=%v", len(data), err)
	}
}

func TestReleaseRejectsForeignTenantAndCorruptBytes(t *testing.T) {
	f := newReleaseFixture(t)
	store, err := artifact.NewDirectoryStore(f.root)
	if err != nil {
		t.Fatal(err)
	}
	req := ReleaseRequest{Action: "manifest", OS: "linux", Architecture: runtime.GOARCH}
	f.release.Labels[tenant.IDLabel] = "other"
	if err := f.reporter.Client.Update(context.Background(), f.release); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.reporter.Release(context.Background(), f.identity, req, store); !errors.Is(err, ErrReleaseIdentity) {
		t.Fatalf("foreign tenant accepted: %v", err)
	}
	f.release.Labels[tenant.IDLabel] = f.identity.TenantID
	if err := f.reporter.Client.Update(context.Background(), f.release); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "agent"), []byte("damaged"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.reporter.Release(context.Background(), f.identity, req, store); !errors.Is(err, artifact.ErrDigest) {
		t.Fatalf("corrupt bytes accepted: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "agent"), f.data, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := f.reporter.Release(ctx, f.identity, req, store); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled release accepted: %v", err)
	}
}

func TestReleaseManifestAndDownloadAreExactAndScoped(t *testing.T) {
	f := newReleaseFixture(t)
	store, err := artifact.NewDirectoryStore(f.root)
	if err != nil {
		t.Fatal(err)
	}
	manifest, data, err := f.reporter.Release(context.Background(), f.identity, ReleaseRequest{Action: "manifest", OS: "linux", Architecture: runtime.GOARCH}, store)
	if err != nil || data != nil || manifest.UID != string(f.release.UID) || manifest.Bytes != int64(len(f.data)) {
		t.Fatalf("manifest = %#v bytes=%d err=%v", manifest, len(data), err)
	}
	got, downloaded, err := f.reporter.Release(context.Background(), f.identity, ReleaseRequest{Action: "download", OS: "linux", Architecture: runtime.GOARCH, ReleaseUID: manifest.UID, SHA256: manifest.SHA256}, store)
	if err != nil || got == nil || string(downloaded) != string(f.data) {
		t.Fatalf("download = %#v bytes=%d err=%v", got, len(downloaded), err)
	}
	_, bytes, err := f.reporter.Release(context.Background(), f.identity, ReleaseRequest{Action: "download", OS: "linux", Architecture: runtime.GOARCH, ReleaseUID: "old", SHA256: manifest.SHA256}, store)
	if !errors.Is(err, ErrReleaseIdentity) || bytes != nil {
		t.Fatalf("stale download accepted: bytes=%d err=%v", len(bytes), err)
	}
	if _, _, err := f.reporter.Release(context.Background(), f.identity, ReleaseRequest{Action: "manifest", OS: "linux", Architecture: runtime.GOARCH, ReleaseUID: manifest.UID}, store); !errors.Is(err, ErrReleaseIdentity) {
		t.Fatalf("manifest selector accepted: %v", err)
	}
}

type mutatingStore struct {
	client client.Client
	node   *ext.ContainerNode
	data   []byte
}

func (s mutatingStore) Load(ctx context.Context, spec ext.AgentReleaseSpec) ([]byte, error) {
	var current ext.ContainerNode
	if err := s.client.Get(ctx, client.ObjectKeyFromObject(s.node), &current); err != nil {
		return nil, err
	}
	current.Annotations[CredentialAnnotation] = "rotated"
	if err := s.client.Update(ctx, &current); err != nil {
		return nil, err
	}
	return s.data, nil
}

func TestReleaseRechecksCredentialAfterArtifactRead(t *testing.T) {
	f := newReleaseFixture(t)
	if _, _, err := f.reporter.Release(context.Background(), f.identity, ReleaseRequest{Action: "manifest", OS: "linux", Architecture: runtime.GOARCH}, mutatingStore{client: f.reporter.Client, node: &ext.ContainerNode{ObjectMeta: metav1.ObjectMeta{Name: f.identity.Name, Namespace: f.identity.Namespace}}, data: f.data}); !errors.Is(err, ErrIdentity) {
		t.Fatalf("credential changed during download was accepted: %v", err)
	}
}
