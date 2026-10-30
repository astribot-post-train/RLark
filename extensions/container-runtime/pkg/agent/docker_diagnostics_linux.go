//go:build linux

package agent

import (
	"context"
	"errors"
	dockerruntime "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/docker"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	"time"
)

// RunDockerDiagnostics runs independently of lifecycle reconciliation so body
// reads cannot hold up cleanup. Each poll/read/complete step has a 10s deadline.
func RunDockerDiagnostics(ctx context.Context, c *Client, provider func(context.Context, rt.TaskIdentity) (*dockerruntime.ScopedProvider, error)) {
	if c == nil || provider == nil {
		return
	}
	delay := 2 * time.Second
	for ctx.Err() == nil {
		step, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := RespondDockerDiagnostics(step, c, func(ctx context.Context, kind string, t rt.TaskIdentity) (raw.LogSnapshot, dockerruntime.StatsSnapshot, error) {
			p, e := provider(ctx, t)
			if e != nil {
				return raw.LogSnapshot{}, dockerruntime.StatsSnapshot{}, e
			}
			if kind == "logs" {
				v, e := p.Logs(ctx, t, raw.MaxLogBytes)
				return v, dockerruntime.StatsSnapshot{}, e
			}
			if kind == "stats" {
				v, e := p.Stats(ctx, t)
				return raw.LogSnapshot{}, v, e
			}
			return raw.LogSnapshot{}, dockerruntime.StatsSnapshot{}, errors.New("unsupported Docker diagnostic kind")
		})
		cancel()
		if err == nil {
			delay = 2 * time.Second
		} else if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
