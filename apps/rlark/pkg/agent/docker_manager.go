package agent

import (
	"context"
	"fmt"
	"sync"

	"golang.org/x/sync/errgroup"

	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// dockerManager is the small local runnable host used by Docker agents. The
// management API still uses controller-runtime's manager; the Docker daemon
// has no Kubernetes watch API and therefore only needs explicit runnables.
type dockerManager struct {
	mu        sync.Mutex
	runnables []manager.Runnable
}

func (m *dockerManager) Add(r manager.Runnable) error {
	if r == nil {
		return nil
	}
	m.mu.Lock()
	m.runnables = append(m.runnables, r)
	m.mu.Unlock()
	return nil
}

func (m *dockerManager) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("docker-manager")
	m.mu.Lock()
	runnables := append([]manager.Runnable(nil), m.runnables...)
	m.mu.Unlock()
	logger.Info("starting Docker local runnables", "count", len(runnables))
	group, runCtx := errgroup.WithContext(ctx)
	for _, runnable := range runnables {
		group.Go(func() error {
			logger.V(1).Info("starting Docker local runnable", "type", fmt.Sprintf("%T", runnable))
			err := runnable.Start(runCtx)
			if err != nil {
				logger.Error(err, "Docker local runnable stopped")
			}
			return err
		})
	}
	if len(runnables) == 0 {
		<-ctx.Done()
		return nil
	}
	return group.Wait()
}
