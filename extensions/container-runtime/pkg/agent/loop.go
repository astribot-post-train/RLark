package agent

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"time"
)

// Observer reads local state without starting workloads.
type Observer interface {
	Observe(context.Context) (Observation, error)
}

// Heartbeater sends an idempotent observation, scoped by authenticated identity.
type Heartbeater interface {
	Heartbeat(context.Context, Observation) error
}

// Run observes before every send, retries outages with bounded exponential
// backoff and jitter, and never resends a stale successful observation.
func Run(ctx context.Context, observer Observer, client Heartbeater, interval, retryMax time.Duration, logger *slog.Logger) error {
	return run(ctx, observer, client, interval, retryMax, logger, nil)
}

func run(ctx context.Context, observer Observer, client Heartbeater, interval, retryMax time.Duration, logger *slog.Logger, observationFailed func(context.Context) error) error {
	if observer == nil || client == nil || logger == nil || interval <= 0 || retryMax < interval || retryMax > 5*time.Minute {
		return errors.New("observer, client, logger and valid heartbeat/retry intervals required")
	}
	initial := time.Second
	if initial > retryMax {
		initial = retryMax
	}
	delay := initial
	for {
		if ctx.Err() != nil {
			return nil
		}
		observation, err := observer.Observe(ctx)
		if err == nil {
			err = client.Heartbeat(ctx, observation)
		} else if observationFailed != nil {
			err = errors.Join(err, observationFailed(ctx))
		}
		if ctx.Err() != nil {
			return nil
		}
		wait := interval
		if err == nil {
			delay = initial
		} else {
			// Subtractive jitter keeps the maximum delay a strict upper bound.
			wait = delay - time.Duration(rand.Int63n(int64(delay/5)+1))
			logger.Warn("container-runtime Agent observation/heartbeat failed; retrying", "error", err, "retry_in", wait)
			if delay < retryMax {
				delay *= 2
				if delay > retryMax {
					delay = retryMax
				}
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
