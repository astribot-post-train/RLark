package stationidentity

import (
	"testing"
	"time"
)

func TestJoinCodeTTLRejectsOverflow(t *testing.T) {
	// Multiplying this input first would wrap the duration to ten minutes.
	const wrapped = (1 << 55) + 600
	if _, err := JoinCodeTTL(wrapped); err == nil {
		t.Fatal("accepted a lifetime that fits only after integer overflow")
	}
	for seconds, want := range map[int64]time.Duration{0: 2 * time.Hour, 600: 10 * time.Minute, 604800: 7 * 24 * time.Hour} {
		ttl, err := JoinCodeTTL(seconds)
		if err != nil || ttl != want {
			t.Errorf("JoinCodeTTL(%d) = %v, %v, want %v", seconds, ttl, err, want)
		}
	}
}
