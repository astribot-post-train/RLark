package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

type observerFunc func(context.Context) (Observation, error)

func (f observerFunc) Observe(ctx context.Context) (Observation, error) { return f(ctx) }

type heartbeatFunc func(context.Context, Observation) error

func (f heartbeatFunc) Heartbeat(ctx context.Context, o Observation) error { return f(ctx, o) }

func TestRunRecoversWithoutReplayingStaleObservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	reads, sends := 0, 0
	observer := observerFunc(func(context.Context) (Observation, error) {
		reads++
		if reads == 1 {
			return Observation{}, errors.New("supervisor unavailable")
		}
		return Observation{Busy: reads == 2}, nil
	})
	sender := heartbeatFunc(func(_ context.Context, o Observation) error {
		sends++
		if sends == 1 {
			if !o.Busy {
				t.Fatal("wrong initial observation")
			}
			return errors.New("API unavailable")
		}
		if o.Busy {
			t.Fatal("replayed stale occupancy")
		}
		cancel()
		return nil
	})
	if err := Run(ctx, observer, sender, time.Millisecond, 2*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	if reads != 3 || sends != 2 {
		t.Fatalf("did not recover: reads=%d sends=%d", reads, sends)
	}
}
func TestRunCanceledBeforeObservation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	observer := observerFunc(func(context.Context) (Observation, error) {
		t.Fatal("observed after cancellation")
		return Observation{}, nil
	})
	sender := heartbeatFunc(func(context.Context, Observation) error { t.Fatal("sent after cancellation"); return nil })
	if err := Run(ctx, observer, sender, time.Second, time.Minute, slog.Default()); err != nil {
		t.Fatal(err)
	}
}
