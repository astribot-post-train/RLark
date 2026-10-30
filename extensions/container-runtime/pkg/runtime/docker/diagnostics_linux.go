//go:build linux

package docker

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw"
	"github.com/docker/docker/api/types/container"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
)

// diagnosticID proves local ownership while holding the execution gate. Body
// reads happen after unlocking so a stalled daemon cannot hold up cleanup.
func (p *ScopedProvider) diagnosticID(ctx context.Context, task rt.TaskIdentity) (string, error) {
	if p == nil || p.Journal == nil || validateTask(task) != nil || task.Generation < 1 {
		return "", errors.New("valid recorded Docker Task required")
	}
	unlock, err := p.Journal.acquire(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()
	if err := p.checkEngine(ctx); err != nil {
		return "", err
	}
	r, err := p.Journal.read(task)
	if err != nil {
		return "", err
	}
	if r == nil || r.Task != task || !r.Creating || r.ContainerID == "" || r.Cancelled || r.Cleaned {
		return "", errors.New("docker diagnostics record unavailable")
	}
	c, err := p.Client.ContainerInspect(ctx, r.ContainerID)
	if err != nil {
		return "", err
	}
	if c.ContainerJSONBase == nil || c.ID != r.ContainerID || !owned(c, task) || c.Config == nil || c.Config.Tty {
		return "", errors.New("docker diagnostics identity unavailable")
	}
	return r.ContainerID, nil
}

// Logs reads bounded, non-follow Engine output for the journal's exact ID.
func (p *ScopedProvider) Logs(ctx context.Context, task rt.TaskIdentity, limit int) (raw.LogSnapshot, error) {
	if limit < 1 || limit > raw.MaxLogBytes {
		return raw.LogSnapshot{}, errors.New("invalid Docker log bound")
	}
	id, err := p.diagnosticID(ctx, task)
	if err != nil {
		return raw.LogSnapshot{}, err
	}
	body, err := p.Client.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true, Tail: "all"})
	if err != nil {
		return raw.LogSnapshot{}, err
	}
	if body == nil {
		return raw.LogSnapshot{}, errors.New("missing Docker logs")
	}
	defer func() { _ = body.Close() }()
	snapshot, err := readMultiplexLogs(body, limit)
	if err != nil {
		return raw.LogSnapshot{}, err
	}
	current, err := p.diagnosticID(ctx, task)
	if err != nil {
		return raw.LogSnapshot{}, err
	}
	if current != id {
		return raw.LogSnapshot{}, errors.New("docker log identity changed")
	}
	return snapshot, nil
}

func readMultiplexLogs(body io.Reader, limit int) (raw.LogSnapshot, error) {
	out := raw.LogSnapshot{Data: []byte{}}
	// Bound header-only frames and total wire bytes as well as decoded output.
	r := &io.LimitedReader{R: body, N: 1 << 20}
	for frames := 0; frames < 4096; frames++ {
		var h [8]byte
		n, err := io.ReadFull(r, h[:])
		if err == io.EOF && n == 0 && r.N > 0 {
			return out, nil
		}
		if err != nil {
			return raw.LogSnapshot{}, errors.New("invalid Docker log framing")
		}
		if (h[0] != 1 && h[0] != 2) || h[1] != 0 || h[2] != 0 || h[3] != 0 {
			return raw.LogSnapshot{}, errors.New("invalid Docker log stream")
		}
		size := uint64(binary.BigEndian.Uint32(h[4:]))
		remaining := limit - len(out.Data)
		take := size
		if take > uint64(remaining) {
			take = uint64(remaining)
		}
		chunk := make([]byte, int(take))
		if _, err := io.ReadFull(r, chunk); err != nil {
			return raw.LogSnapshot{}, errors.New("incomplete Docker log frame")
		}
		out.Data = append(out.Data, chunk...)
		if size > take {
			out.Truncated = true
			return out, nil
		}
	}
	return raw.LogSnapshot{}, errors.New("docker log framing limit reached")
}

// Stats returns one bounded Linux sample; it never calculates percentages.
func (p *ScopedProvider) Stats(ctx context.Context, task rt.TaskIdentity) (StatsSnapshot, error) {
	id, err := p.diagnosticID(ctx, task)
	if err != nil {
		return StatsSnapshot{}, err
	}
	response, err := p.Client.ContainerStatsOneShot(ctx, id)
	if err != nil {
		return StatsSnapshot{}, err
	}
	if response.Body == nil {
		return StatsSnapshot{}, errors.New("missing Docker stats")
	}
	defer func() { _ = response.Body.Close() }()
	if response.OSType != "" && response.OSType != "linux" {
		return StatsSnapshot{}, errors.New("linux Docker stats required")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return StatsSnapshot{}, err
	}
	if len(data) > 1<<20 {
		return StatsSnapshot{}, errors.New("docker stats response exceeds limit")
	}
	snapshot, err := decodeStats(data, id)
	if err != nil {
		return StatsSnapshot{}, err
	}
	current, err := p.diagnosticID(ctx, task)
	if err != nil {
		return StatsSnapshot{}, err
	}
	if current != id {
		return StatsSnapshot{}, errors.New("docker stats identity changed")
	}
	return snapshot, nil
}

func decodeStats(data []byte, id string) (StatsSnapshot, error) {
	var in struct {
		ID   string    `json:"id"`
		Read time.Time `json:"read"`
		CPU  struct {
			Usage struct {
				Total *uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
		} `json:"cpu_stats"`
		Memory struct {
			Usage *uint64 `json:"usage"`
			Limit *uint64 `json:"limit"`
		} `json:"memory_stats"`
		PIDs struct {
			Current *uint64 `json:"current"`
		} `json:"pids_stats"`
		Block struct {
			Bytes *[]struct {
				Op    string  `json:"op"`
				Value *uint64 `json:"value"`
			} `json:"io_service_bytes_recursive"`
		} `json:"blkio_stats"`
	}
	if err := json.Unmarshal(data, &in); err != nil {
		return StatsSnapshot{}, err
	}
	if id == "" || in.ID != id || in.Read.IsZero() {
		return StatsSnapshot{}, errors.New("invalid Docker stats identity")
	}
	counter := func(n *uint64) string {
		if n == nil {
			return ""
		}
		return strconv.FormatUint(*n, 10)
	}
	out := StatsSnapshot{SampledAt: in.Read.UTC().Format(time.RFC3339Nano), CPUUsageNS: counter(in.CPU.Usage.Total), MemoryUsageBytes: counter(in.Memory.Usage), MemoryLimitBytes: counter(in.Memory.Limit), PIDs: counter(in.PIDs.Current)}
	if in.Block.Bytes != nil {
		var reads, writes uint64
		var hasReads, hasWrites bool
		for _, e := range *in.Block.Bytes {
			if e.Op == "" || e.Value == nil {
				return StatsSnapshot{}, errors.New("invalid Docker block counter")
			}
			var dest *uint64
			switch strings.ToLower(e.Op) {
			case "read":
				dest = &reads
				hasReads = true
			case "write":
				dest = &writes
				hasWrites = true
			default:
				continue
			}
			if math.MaxUint64-*dest < *e.Value {
				return StatsSnapshot{}, errors.New("docker block counter overflow")
			}
			*dest += *e.Value
		}
		if hasReads {
			out.BlockReadBytes = strconv.FormatUint(reads, 10)
		}
		if hasWrites {
			out.BlockWriteBytes = strconv.FormatUint(writes, 10)
		}
	}
	if err := out.Validate(time.Now()); err != nil {
		return StatsSnapshot{}, err
	}
	return out, nil
}
