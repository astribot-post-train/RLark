package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/provision"
)

func TestDecodeRejectsInvalidAndOversizedRequests(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"valid", `{"name":"cloud","runtime":"Raw"}`, true},
		{"unknown field", `{"name":"cloud","unknown":true}`, false},
		{"multiple values", `{} {}`, false},
		{"trailing garbage", `{} invalid`, false},
		{"malformed", `{"name":`, false},
		{"oversized trailing whitespace", `{}` + strings.Repeat(" ", 1<<20), false},
		{"hidden trailing value", `{}` + strings.Repeat(" ", 1<<20) + `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "request.json")
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			var request provision.ClusterRequest
			if err := decode(path, &request); (err == nil) != tc.valid {
				t.Fatalf("decode error = %v, want valid = %v", err, tc.valid)
			}
		})
	}
}
