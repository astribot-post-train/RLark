package docker

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// StatsSnapshot is a transient Linux Engine sample. Empty counters are absent;
// decimal strings preserve uint64 precision in browser clients.
type StatsSnapshot struct {
	SampledAt        string `json:"sampledAt"`
	CPUUsageNS       string `json:"cpuUsageNS,omitempty"`
	MemoryUsageBytes string `json:"memoryUsageBytes,omitempty"`
	MemoryLimitBytes string `json:"memoryLimitBytes,omitempty"`
	PIDs             string `json:"pids,omitempty"`
	BlockReadBytes   string `json:"blockReadBytes,omitempty"`
	BlockWriteBytes  string `json:"blockWriteBytes,omitempty"`
}

// Validate rejects malformed or future samples without inventing missing data.
func (s StatsSnapshot) Validate(now time.Time) error {
	t, err := time.Parse(time.RFC3339Nano, s.SampledAt)
	if err != nil || !strings.HasSuffix(s.SampledAt, "Z") || t.IsZero() || t.After(now.Add(time.Minute)) {
		return errors.New("invalid Docker stats timestamp")
	}
	for _, v := range []string{s.CPUUsageNS, s.MemoryUsageBytes, s.MemoryLimitBytes, s.PIDs, s.BlockReadBytes, s.BlockWriteBytes} {
		if v == "" {
			continue
		}
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil || strconv.FormatUint(n, 10) != v {
			return errors.New("invalid Docker stats counter")
		}
	}
	return nil
}
