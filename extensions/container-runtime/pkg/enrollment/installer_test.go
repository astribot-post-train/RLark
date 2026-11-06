package enrollment

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func installerArchive(t *testing.T, identity string, mutate func(map[string][]byte), extra *tar.Header) []byte {
	t.Helper()
	config, _ := json.Marshal(map[string]any{"identity": identity, "stateDirectory": "/unused", "services": map[string][]string{"agent": {"/bin/true"}}})
	files := map[string][]byte{"services.json": config, "node-services.py": []byte("import json; print(json.dumps({'identity':'test','services':['agent','task-manager']}))\n"), "container-runtime-agent": []byte("test-agent")}
	var sums []string
	for name, b := range files {
		sums = append(sums, fmt.Sprintf("%x  %s", sha256.Sum256(b), name))
	}
	files["SHA256SUMS"] = []byte(strings.Join(sums, "\n") + "\n")
	if mutate != nil {
		mutate(files)
	}
	var out bytes.Buffer
	w := tar.NewWriter(&out)
	for name, b := range files {
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(b))}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	if extra != nil {
		if err := w.WriteHeader(extra); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func runInstaller(t *testing.T, path string, data []byte) error {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 required")
	}
	cmd := exec.Command(python, "-c", installer, path, "test", python)
	cmd.Stdin = bytes.NewReader(data)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, out.String())
	}
	return nil
}
func TestInstallerIdempotenceAndForeignIdentity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "installed")
	data := installerArchive(t, "test", nil, nil)
	if err := runInstaller(t, root, data); err != nil {
		t.Fatal(err)
	}
	if err := runInstaller(t, root, data); err != nil {
		t.Fatal(err)
	}
	if err := runInstaller(t, root, installerArchive(t, "foreign", nil, nil)); err == nil {
		t.Fatal("foreign identity accepted")
	}
	changed := installerArchive(t, "test", func(files map[string][]byte) {
		files["services.json"] = []byte(`{"identity":"test","services":{},"stateDirectory":"/foreign"}`)
	}, nil)
	if err := runInstaller(t, root, changed); err == nil {
		t.Fatal("modified services bypassed checksums")
	}
}
func TestInstallerRejectsArchiveTraversalAndLinksBeforeInstall(t *testing.T) {
	for _, header := range []*tar.Header{{Name: "../escape", Mode: 0600}, {Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/tmp/unknown"}, {Name: "services.json", Mode: 0600}} {
		t.Run(header.Name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "installed")
			if err := runInstaller(t, root, installerArchive(t, "test", nil, header)); err == nil {
				t.Fatal("unsafe archive accepted")
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatal("failed archive installed")
			}
		})
	}
}
func TestInstallerRefusesUnknownExistingDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "unknown")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "unowned")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runInstaller(t, root, installerArchive(t, "test", nil, nil)); err == nil {
		t.Fatal("unknown root adopted")
	}
	b, err := os.ReadFile(marker)
	if err != nil || string(b) != "keep" {
		t.Fatal("unknown root modified")
	}
}
