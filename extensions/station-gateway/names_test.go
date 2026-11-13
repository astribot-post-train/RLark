package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteNamesKeepsForeignLinesAndTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(path, []byte("127.0.0.1 localhost\n10.0.0.9 cp.old # rlark names\n10.0.0.1 registry # rlark registry-trust\n"), 0644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	names := []hostName{{Name: "cp.c1.rlark.internal", Address: "10.254.240.5"}, {Name: "mgmt.rlark.internal", Address: "10.254.240.1"}}
	for range 2 {
		if err := writeNames(path, names); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := os.ReadFile(path)
	want := "127.0.0.1 localhost\n10.0.0.1 registry # rlark registry-trust\n10.254.240.5 cp.c1.rlark.internal # rlark names\n10.254.240.1 mgmt.rlark.internal # rlark names\n"
	if string(got) != want {
		t.Fatalf("hosts:\n%s", got)
	}
	if after, _ := os.Stat(path); !os.SameFile(before, after) {
		t.Fatal("the hosts file was replaced, not rewritten in place")
	}
}
