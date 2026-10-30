//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw/supervision"
)

// LocalObserver reports Raw node observations, never GPU entitlement. A missing
// supervisor, failed GPU query or uncertain resource boundary prevents a report.
type LocalObserver struct {
	Version          string
	PrivateAddress   string
	NvidiaSMI        string
	CgroupMount      string
	SupervisorSocket string
	EnableUpgrades   bool
}

// Observe reads current occupancy before sending an observation. The server's
// exclusive allocation is still required: an idle observation is not a lock.
func (o LocalObserver) Observe(ctx context.Context) (Observation, error) {
	if strings.TrimSpace(o.Version) == "" || strings.TrimSpace(o.PrivateAddress) == "" || o.SupervisorSocket == "" {
		return Observation{}, errors.New("agent version, private address and Raw supervisor socket are required")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client, err := supervision.NewClient(o.SupervisorSocket)
	if err != nil {
		return Observation{}, fmt.Errorf("configure supervisor status: %w", err)
	}
	defer client.CloseIdleConnections()
	status, err := client.Status(ctx)
	if err != nil {
		return Observation{}, fmt.Errorf("observe supervisor status: %w", err)
	}
	capacity, err := ReadCgroupCapacity(o.CgroupMount)
	if err != nil {
		return Observation{}, err
	}
	gpus, err := visibleGPUs(ctx, o.NvidiaSMI)
	if err != nil {
		return Observation{}, err
	}
	obs := Observation{Version: o.Version, PrivateAddress: o.PrivateAddress, Capacity: capacity, VisibleGPUUUIDs: gpus, Busy: status.Busy}
	if o.EnableUpgrades {
		obs.Capabilities = []string{UpgradeCapability, "agent-platform/" + runtime.GOOS + "/" + runtime.GOARCH}
	}
	return obs, nil
}

var gpuUUID = regexp.MustCompile(`^GPU-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func visibleGPUs(ctx context.Context, executable string) ([]string, error) {
	if executable == "" {
		executable = "nvidia-smi"
	}
	command := exec.CommandContext(ctx, executable, "--query-gpu=uuid", "--format=csv,noheader,nounits")
	command.WaitDelay = time.Second
	var output boundedOutput
	command.Stdout, command.Stderr = &output, io.Discard
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("observe GPU visibility: %w", err)
	}
	return parseGPUs(string(output.data))
}

func parseGPUs(output string) ([]string, error) {
	seen := map[string]bool{}
	var result []string
	for _, line := range strings.Split(output, "\n") {
		value := strings.TrimSpace(line)
		if value == "" {
			continue
		}
		if !gpuUUID.MatchString(value) || seen[value] || len(result) == 256 {
			return nil, errors.New("invalid, duplicate or excessive visible GPU UUIDs")
		}
		seen[value] = true
		result = append(result, value)
	}
	return result, nil
}

type boundedOutput struct{ data []byte }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(b.data)+len(p) > 64<<10 {
		return 0, errors.New("GPU query output exceeds limit")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}
