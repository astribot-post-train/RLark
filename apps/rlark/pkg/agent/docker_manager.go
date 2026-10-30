package agent

import (
	"context"
	"sync"

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
	m.mu.Lock()
	runnables := append([]manager.Runnable(nil), m.runnables...)
	m.mu.Unlock()
	for _, runnable := range runnables {
		if err := runnable.Start(ctx); err != nil {
			return err
		}
	}
	<-ctx.Done()
	return nil
}
