package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
)

func testSpec(path string, data []byte) ext.AgentReleaseSpec {
	digest := sha256.Sum256(data)
	return ext.AgentReleaseSpec{Version: "v1", OS: "linux", Architecture: runtime.GOARCH, SHA256: hex.EncodeToString(digest[:]), ArtifactPath: path, Protocol: "v1"}
}

func testBinary(t *testing.T) []byte {
	t.Helper()
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("supported Linux ELF packaging")
	}
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestDirectoryStoreRecoversMissingAndCorruptedArtifacts(t *testing.T) {
	data := testBinary(t)
	root := t.TempDir()
	store, err := NewDirectoryStore(root)
	if err != nil {
		t.Fatal(err)
	}
	spec := testSpec("releases/agent", data)
	if _, err := store.Load(context.Background(), spec); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing artifact = %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "releases"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "releases", "agent")
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(context.Background(), spec)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("verified load = %v, %d bytes", err, len(got))
	}
	if err := os.WriteFile(path, []byte("corrupted"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), spec); !errors.Is(err, ErrDigest) {
		t.Fatalf("corruption not rejected: %v", err)
	}
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), spec); err != nil {
		t.Fatalf("restored artifact not recovered: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Load(ctx, spec); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read = %v", err)
	}
}

func TestDirectoryStoreRejectsSymlinkComponentsAndDirectories(t *testing.T) {
	data := testBinary(t)
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "agent"), data, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"file-link", "dir-link"} {
		target := filepath.Join(outside, "agent")
		if name == "dir-link" {
			target = outside
		}
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := NewDirectoryStore(filepath.Join(root, "dir-link")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("symlink root accepted: %v", err)
	}
	store, err := NewDirectoryStore(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"file-link", "dir-link/agent", "dir-link"} {
		if _, err := store.Load(context.Background(), testSpec(path, data)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("symlink %q accepted: %v", path, err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "directory"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), testSpec("directory", data)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("directory accepted as artifact: %v", err)
	}
}

func TestArtifactValidationRejectsUnsafeMetadataAndNonExecutables(t *testing.T) {
	data := testBinary(t)
	for _, change := range []func(*ext.AgentReleaseSpec){
		func(s *ext.AgentReleaseSpec) { s.ArtifactPath = "../agent" },
		func(s *ext.AgentReleaseSpec) { s.ArtifactPath = "/agent" },
		func(s *ext.AgentReleaseSpec) { s.ArtifactPath = "a//agent" },
		func(s *ext.AgentReleaseSpec) { s.ArtifactPath = "a/./agent" },
		func(s *ext.AgentReleaseSpec) { s.ArtifactPath = `a\agent` },
		func(s *ext.AgentReleaseSpec) { s.ArtifactPath = "." },
		func(s *ext.AgentReleaseSpec) { s.Protocol = "v2" },
		func(s *ext.AgentReleaseSpec) { s.OS = "windows" },
		func(s *ext.AgentReleaseSpec) { s.Architecture = "ppc64" },
		func(s *ext.AgentReleaseSpec) { s.Version = "\n" },
		func(s *ext.AgentReleaseSpec) { s.SHA256 = "BAD" },
	} {
		spec := testSpec("agent", data)
		change(&spec)
		if err := ValidateSpec(spec); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid metadata accepted: %+v %v", spec, err)
		}
	}
	wrong := testSpec("agent", data)
	wrong.Architecture = "arm64"
	if runtime.GOARCH == "arm64" {
		wrong.Architecture = "amd64"
	}
	if err := Verify(wrong, data); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong executable architecture accepted: %v", err)
	}
	script := []byte("#!/bin/sh\nexit 0\n")
	if err := Verify(testSpec("agent", script), script); !errors.Is(err, ErrInvalid) {
		t.Fatalf("script accepted: %v", err)
	}
	object := bytes.Clone(data)
	object[16], object[17] = 1, 0 // ELF ET_REL, preserving its architecture.
	if err := Verify(testSpec("agent", object), object); !errors.Is(err, ErrInvalid) {
		t.Fatalf("relocatable object accepted: %v", err)
	}
}

func TestDirectoryStoreRejectsOversizedFileWithoutReadingIt(t *testing.T) {
	root := t.TempDir()
	f, err := os.Create(filepath.Join(root, "large"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := NewDirectoryStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), testSpec("large", []byte("x"))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized file accepted: %v", err)
	}
}
