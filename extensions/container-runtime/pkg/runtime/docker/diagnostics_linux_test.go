//go:build linux

package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/require"
)

type diagnosticEngine struct {
	*journalEngine
	logs  func(context.Context) (io.ReadCloser, error)
	stats func(context.Context) (types.ContainerStats, error)
	ids   []string
}

func (e *diagnosticEngine) ContainerLogs(ctx context.Context, id string, opts container.LogsOptions) (io.ReadCloser, error) {
	e.ids = append(e.ids, id)
	if opts.Follow || !opts.ShowStdout || !opts.ShowStderr || opts.Tail != "all" {
		return nil, errors.New("invalid log options")
	}
	return e.logs(ctx)
}
func (e *diagnosticEngine) ContainerStatsOneShot(ctx context.Context, id string) (types.ContainerStats, error) {
	e.ids = append(e.ids, id)
	return e.stats(ctx)
}
func logFrame(stream byte, data string) []byte {
	frame := make([]byte, 8+len(data))
	frame[0] = stream
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(data)))
	copy(frame[8:], data)
	return frame
}
func TestDockerMultiplexLogsBoundAndRejectMalformedResponses(t *testing.T) {
	wire := append(logFrame(1, "out\n"), logFrame(2, "err\n")...)
	got, err := readMultiplexLogs(bytes.NewReader(wire), 20)
	require.NoError(t, err)
	require.Equal(t, []byte("out\nerr\n"), got.Data)
	require.False(t, got.Truncated)
	got, err = readMultiplexLogs(bytes.NewReader(wire), 6)
	require.NoError(t, err)
	require.Equal(t, []byte("out\ner"), got.Data)
	require.True(t, got.Truncated)
	for _, bad := range [][]byte{wire[:3], wire[:9], logFrame(0, "bad"), append([]byte{1, 1, 0, 0, 0, 0, 0, 0}, logFrame(1, "x")...), bytes.Repeat(logFrame(1, ""), 4097)} {
		_, err := readMultiplexLogs(bytes.NewReader(bad), 20)
		require.Error(t, err)
	}
}
func TestDockerStatsPreserveMissingAndZeroCounters(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	prefix := fmt.Sprintf(`{"id":"exact","read":%q,`, now)
	got, err := decodeStats([]byte(prefix+`"cpu_stats":{"cpu_usage":{"total_usage":18446744073709551615}},"memory_stats":{"usage":0},"blkio_stats":{"io_service_bytes_recursive":[{"op":"Read","value":0},{"op":"Write","value":2},{"op":"Write","value":3}]}}`), "exact")
	require.NoError(t, err)
	require.Equal(t, "18446744073709551615", got.CPUUsageNS)
	require.Equal(t, "0", got.MemoryUsageBytes)
	require.Empty(t, got.MemoryLimitBytes)
	require.Empty(t, got.PIDs)
	require.Equal(t, "0", got.BlockReadBytes)
	require.Equal(t, "5", got.BlockWriteBytes)
	got, err = decodeStats([]byte(prefix+`"blkio_stats":{"io_service_bytes_recursive":[]}}`), "exact")
	require.NoError(t, err)
	require.Empty(t, got.BlockReadBytes)
	require.Empty(t, got.BlockWriteBytes)
	for _, bad := range []string{
		`{"id":"other","read":` + fmt.Sprintf("%q}", now),
		prefix + `"cpu_stats":{"cpu_usage":{"total_usage":-1}}}`,
		prefix + `"pids_stats":{"current":18446744073709551616}}`,
		prefix + `"blkio_stats":{"io_service_bytes_recursive":[{"op":"Read"}]}}`,
		prefix + `"blkio_stats":{"io_service_bytes_recursive":[{"value":1}]}}`,
		prefix + `"blkio_stats":{"io_service_bytes_recursive":[{"op":"Read","value":18446744073709551615},{"op":"Read","value":1}]}}`,
		`{"id":"exact","read":"2099-01-01T00:00:00Z"}`, `{}`, `null`, prefix + `"memory_stats":{"usage":1.5}}`,
	} {
		_, err := decodeStats([]byte(bad), "exact")
		require.Error(t, err, bad)
	}
	for _, counter := range []string{"-1", "+1", "01", "1.0", "18446744073709551616"} {
		require.Error(t, (StatsSnapshot{SampledAt: now, PIDs: counter}).Validate(time.Now()))
	}
}
func TestDockerDiagnosticsPinJournalIDAndFenceCleanupWithoutBlocking(t *testing.T) {
	for _, kind := range []string{"logs", "stats"} {
		t.Run(kind, func(t *testing.T) {
			p, engine, req, _ := scopedFixture(t)
			ctx := context.Background()
			_, err := p.Apply(ctx, req)
			require.NoError(t, err)
			entered, release := make(chan struct{}), make(chan struct{})
			d := &diagnosticEngine{journalEngine: engine}
			// The body is stalled, not the inspect/ownership checks. Cleanup must run.
			body := func(data []byte) io.ReadCloser {
				return &blockedDiagnosticBody{Reader: bytes.NewReader(data), entered: entered, release: release}
			}
			d.logs = func(context.Context) (io.ReadCloser, error) { return body(logFrame(1, "private-output")), nil }
			d.stats = func(context.Context) (types.ContainerStats, error) {
				return types.ContainerStats{OSType: "linux", Body: body([]byte(fmt.Sprintf(`{"id":"container-id","read":%q,"memory_stats":{"usage":1}}`, time.Now().UTC().Format(time.RFC3339Nano))))}, nil
			}
			p.Client = d
			result := make(chan error, 1)
			go func() {
				if kind == "logs" {
					_, err := p.Logs(ctx, req.Task, 64)
					result <- err
				} else {
					_, err := p.Stats(ctx, req.Task)
					result <- err
				}
			}()
			<-entered
			cleanup := make(chan error, 1)
			go func() { _, err := p.Cleanup(ctx, req.Task); cleanup <- err }()
			select {
			case err := <-cleanup:
				require.NoError(t, err)
			case <-time.After(time.Second):
				close(release)
				t.Fatal("diagnostic body blocked cleanup")
			}
			close(release)
			require.Error(t, <-result, "payload read during cleanup must be discarded")
			require.Equal(t, []string{"container-id"}, d.ids)
		})
	}
}

type blockedDiagnosticBody struct {
	*bytes.Reader
	entered, release chan struct{}
	started          bool
}

func (b *blockedDiagnosticBody) Read(p []byte) (int, error) {
	if !b.started {
		b.started = true
		close(b.entered)
		<-b.release
	}
	return b.Reader.Read(p)
}
func (b *blockedDiagnosticBody) Close() error { return nil }

func TestDockerDiagnosticsRejectIdentityDriftAndBoundStats(t *testing.T) {
	for _, mode := range []string{"missing record", "generation", "engine", "labels", "tty", "oversized stats", "foreign stats", "valid", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			p, e, req, _ := scopedFixture(t)
			ctx := context.Background()
			if mode != "missing record" {
				_, err := p.Apply(ctx, req)
				require.NoError(t, err)
			}
			d := &diagnosticEngine{journalEngine: e}
			d.logs = func(context.Context) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(logFrame(1, "ok"))), nil
			}
			data := fmt.Sprintf(`{"id":"container-id","read":%q,"pids_stats":{"current":0}}`, time.Now().UTC().Format(time.RFC3339Nano))
			switch mode {
			case "generation":
				req.Task.Generation++
			case "engine":
				p.EngineID = "other"
			case "labels":
				e.current.Config.Labels[TaskLabel] = "foreign"
			case "tty":
				e.current.Config.Tty = true
			case "oversized stats":
				data = strings.Repeat(" ", (1<<20)+1)
			case "foreign stats":
				data = strings.Replace(data, "container-id", "foreign", 1)
			case "cancelled":
				_, err := p.Cleanup(ctx, req.Task)
				require.NoError(t, err)
			}
			d.stats = func(context.Context) (types.ContainerStats, error) {
				return types.ContainerStats{Body: io.NopCloser(strings.NewReader(data)), OSType: "linux"}, nil
			}
			p.Client = d
			_, err := p.Stats(ctx, req.Task)
			if mode == "valid" {
				require.NoError(t, err)
				v, err := p.Logs(ctx, req.Task, 64)
				require.NoError(t, err)
				require.Equal(t, []byte("ok"), v.Data)
			} else {
				require.Error(t, err)
			}
		})
	}
}
