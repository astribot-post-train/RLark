package controller

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

type leaderProbe struct {
	id      int
	started chan<- int
	active  *atomic.Int32
	overlap *atomic.Bool
}

func (p *leaderProbe) NeedLeaderElection() bool { return true }

func (p *leaderProbe) Start(ctx context.Context) error {
	if p.active.Add(1) != 1 {
		p.overlap.Store(true)
	}
	p.started <- p.id
	<-ctx.Done()
	p.active.Add(-1)
	return nil
}

// The supplied kubeconfig must reference a disposable, isolated workspace.
func TestLiveLeaderElectionHandoff(t *testing.T) {
	kubeconfig := os.Getenv("STATION_TEST_ISOLATED_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set STATION_TEST_ISOLATED_KUBECONFIG to a disposable workspace")
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "station-leader-test"}}); err != nil {
		t.Fatal(err)
	}
	lease, renew, retry := 4*time.Second, 3*time.Second, 500*time.Millisecond
	started := make(chan int, 2)
	errors := make(chan error, 2)
	var active atomic.Int32
	var overlap atomic.Bool
	var managers sync.WaitGroup
	var cancels [2]context.CancelFunc
	for i := 0; i < 2; i++ {
		mgr, err := ctrl.NewManager(config, ctrl.Options{
			Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
			LeaderElection: true, LeaderElectionID: "station-test.station.rlark.io", LeaderElectionNamespace: "station-leader-test",
			LeaseDuration: &lease, RenewDeadline: &renew, RetryPeriod: &retry,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := mgr.Add(&leaderProbe{id: i, started: started, active: &active, overlap: &overlap}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		managers.Add(1)
		go func() {
			defer managers.Done()
			errors <- mgr.Start(ctx)
		}()
	}
	defer func() { cancels[0](); cancels[1](); managers.Wait() }()
	var first int
	select {
	case first = <-started:
	case err := <-errors:
		t.Fatalf("manager stopped before leadership: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("first leader not elected")
	}
	cancels[first]()
	select {
	case second := <-started:
		if second == first {
			t.Fatal("same manager elected twice")
		}
	case err := <-errors:
		if err != nil {
			t.Fatalf("manager failed during handoff: %v", err)
		}
		select {
		case second := <-started:
			if second == first {
				t.Fatal("same manager elected twice")
			}
		case <-time.After(20 * time.Second):
			t.Fatal("second leader not elected")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("second leader not elected")
	}
	if overlap.Load() {
		t.Fatal("leader-only tasks overlapped")
	}
}
