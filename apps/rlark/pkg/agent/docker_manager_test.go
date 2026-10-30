package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/base"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

func TestDockerManagerStartsAllWorkersAndCancelsOnFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m := &dockerManager{}
	started := make(chan struct{})
	stopped := make(chan struct{})
	want := errors.New("worker failed")
	_ = m.Add(manager.RunnableFunc(func(ctx context.Context) error { close(started); <-ctx.Done(); close(stopped); return nil }))
	_ = m.Add(manager.RunnableFunc(func(ctx context.Context) error {
		select {
		case <-started:
			return want
		case <-ctx.Done():
			return ctx.Err()
		}
	}))
	if err := m.Start(ctx); !errors.Is(err, want) {
		t.Fatalf("err=%v", err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("sibling worker did not stop")
	}
}

func TestDockerTaskControllerDoesNotInstallKubernetesPushWatches(t *testing.T) {
	controller := task.NewTaskController(base.Controller{AgentType: "Docker", LocalDockerClient: struct{}{}})
	if err := controller.SetupPushController(&dockerManager{}); err != nil {
		t.Fatal(err)
	}
}
