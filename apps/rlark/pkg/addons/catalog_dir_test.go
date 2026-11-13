package addons

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An item of the catalog directory is offered and rendered like a built-in
// one, in both layouts, and appears without a restart.
func TestCatalogDirectoryAddsItemsAtRuntime(t *testing.T) {
	dir := t.TempDir()
	r := &registry{addons: map[string]Addon{}, dir: dir}
	if err := r.load(); err != nil {
		t.Fatal(err)
	}
	builtIn := len(r.List())
	write := func(name, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	meta := "name: %s\ndisplayName: Robot\ncategory: robot\nversion: v1\ndescription: d\nparameters:\n- {name: image, displayName: Image, type: string, required: true}\n"
	manifest := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: profile, namespace: {{ .Namespace }}}\ndata: {image: \"{{ .Values.image }}\"}\n"
	write("fleet-robot-s1u/addon.yaml", strings.Replace(meta, "%s", "fleet-robot-s1u", 1))
	write("fleet-robot-s1u/manifests/profile.yaml", manifest)
	// The shape of a mounted ConfigMap: manifests beside addon.yaml, revisions in "..data".
	write("flat/addon.yaml", strings.Replace(meta, "%s", "flat-item", 1))
	write("flat/profile.yaml", manifest)
	write("flat/..data/ignored.yaml", "not: a manifest")
	write("clash/addon.yaml", strings.Replace(meta, "%s", "csi-driver-rclone", 1))

	r.readAt = time.Time{} // the directory was read while it was empty; let it be read again
	if got := len(r.List()); got != builtIn+2 {
		t.Fatalf("catalog has %d items, want %d built-in and 2 from the directory", got, builtIn)
	}
	for _, name := range []string{"fleet-robot-s1u", "flat-item"} {
		addon, ok := r.Get(name)
		if !ok {
			t.Fatalf("%s is not offered", name)
		}
		rendered, err := addon.Render(map[string]string{"image": "registry/robot@sha256:1"}, "rlark-system", name, "uid")
		if err != nil || len(rendered) != 1 || !strings.Contains(string(rendered[0].Raw), "registry/robot@sha256:1") || !strings.Contains(string(rendered[0].Raw), "namespace: rlark-system") {
			t.Fatalf("%s rendered %v %v", name, rendered, err)
		}
	}
	if addon, _ := r.Get("csi-driver-rclone"); addon.(*embeddedAddon).files != manifestsFS {
		t.Fatal("a directory item replaced a built-in one")
	}

	// A later item appears once the directory is read again.
	write("later/addon.yaml", strings.Replace(meta, "%s", "later-item", 1))
	r.readAt = time.Time{}
	if _, ok := r.Get("later-item"); !ok {
		t.Fatal("an item added to the directory is not offered")
	}
}
