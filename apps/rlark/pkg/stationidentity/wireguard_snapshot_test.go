package stationidentity

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWireguardSnapshotFreshnessAndValidation(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "wireguard.json")
	if got := readWireguardSnapshot(path, now); got != nil {
		t.Fatal("missing snapshot was accepted")
	}
	tests := []struct {
		name, body string
		valid      bool
	}{
		{"fresh", fmt.Sprintf(`{"observedAt":%q,"interface":"st-wg0","addresses":["10.254.240.1/32"]}`, now.Format(time.RFC3339)), true},
		{"stale", fmt.Sprintf(`{"observedAt":%q,"interface":"st-wg0","addresses":["10.254.240.1/32"]}`, now.Add(-31*time.Second).Format(time.RFC3339)), false},
		{"future", fmt.Sprintf(`{"observedAt":%q,"interface":"st-wg0","addresses":[]}`, now.Add(6*time.Second).Format(time.RFC3339)), false},
		{"invalid address", fmt.Sprintf(`{"observedAt":%q,"interface":"st-wg0","addresses":["bad"]}`, now.Format(time.RFC3339)), false},
		{"unknown field", fmt.Sprintf(`{"observedAt":%q,"interface":"st-wg0","addresses":[],"privateKey":"secret"}`, now.Format(time.RFC3339)), false},
		{"oversized", string(make([]byte, 16*1024+1)), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			if got := readWireguardSnapshot(path, now); (got != nil) != tc.valid {
				t.Fatalf("accepted = %v, want %v", got != nil, tc.valid)
			}
		})
	}
}
