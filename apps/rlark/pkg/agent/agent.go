package agent

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/rlinf/rlark/api/kubeclients/clientset/versioned"
	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	dockerruntime "github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/runtime/docker"
	"github.com/rlinf/rlark/apps/rlark/pkg/common"
	"github.com/rlinf/rlark/apps/rlark/pkg/server"
	"github.com/rlinf/rlark/apps/rlark/pkg/utils"
)

// Agent is the data plane agent.
type Agent struct {
	config Config

	serverClient *server.Client

	managementConfig *rest.Config
	managementClient versioned.Interface

	localKubeConfig   *rest.Config
	localKubeClient   kubernetes.Interface
	localKubeHandler  http.Handler
	localDockerClient dockerruntime.EngineClient

	localListener net.Listener
	localDialer   utils.Dial
	ready         atomic.Bool
	drainMu       sync.Mutex
	drain         context.CancelFunc
}

// NewAgent creates a new Agent.
func NewAgent(config Config) *Agent {
	return &Agent{
		config: config,
	}
}

func (a *Agent) SetReady(ready bool) {
	a.ready.Store(ready)
}

func (a *Agent) startDrain() {
	a.ready.Store(false)
	a.drainMu.Lock()
	drain := a.drain
	a.drainMu.Unlock()
	if drain != nil {
		drain()
	}
}

func (a *Agent) init(ctx context.Context) error {
	_ = ctx
	var err error
	if err := validateTaskRuntimeResyncPeriod(a.config.Mode, a.config.TaskRuntimeResyncPeriod); err != nil {
		return err
	}
	if err := a.config.ControllerConcurrency.Validate(); err != nil {
		return fmt.Errorf("validate controller concurrency: %w", err)
	}
	if a.config.PodOrphanSweepInterval <= 0 {
		return fmt.Errorf("pod orphan sweep interval must be positive")
	}
	if a.config.PodOrphanSweepPageSize <= 0 {
		return fmt.Errorf("pod orphan sweep page size must be positive")
	}
	if a.config.PodStaleTTL <= 0 {
		return fmt.Errorf("pod stale TTL must be positive")
	}
	if err := a.config.validateRaw(); err != nil {
		return err
	}
	// Initialize server client
	a.serverClient, err = server.NewClientFromConfig(a.config.ClientConfig)
	if err != nil {
		return fmt.Errorf("create server client: %w", err)
	}

	// Initialize Management Kube API client
	if rlarkv1alpha1.AgentType(a.config.AgentType) == rlarkv1alpha1.AgentTypeRaw {
		a.managementConfig, err = a.config.ClientConfig.BuildRawRestConfig()
	} else {
		a.managementConfig, err = a.config.ClientConfig.BuildRestConfig()
	}
	if err != nil {
		return fmt.Errorf("build management API config: %w", err)
	}
	a.managementClient, err = versioned.NewForConfig(a.managementConfig)
	if err != nil {
		return fmt.Errorf("create management API client: %w", err)
	}

	switch rlarkv1alpha1.AgentType(a.config.AgentType) {
	case rlarkv1alpha1.AgentTypeKubernetes:
		// Initialize Kubernetes client
		a.localKubeConfig, err = a.config.KubeClientConfig.BuildRestConfig()
		if err != nil {
			return fmt.Errorf("build Kubernetes REST config: %w", err)
		}
		a.localKubeClient, err = kubernetes.NewForConfig(a.localKubeConfig)
		if err != nil {
			return fmt.Errorf("create Kubernetes client: %w", err)
		}

		// Initialize Kubernetes Proxy
		kubeProxy, err := server.NewKubeProxy(a.localKubeConfig)
		if err != nil {
			return fmt.Errorf("create kube proxy: %w", err)
		}
		a.localKubeHandler = kubeProxy.GetHandler()

	case rlarkv1alpha1.AgentTypeDocker:
		a.localDockerClient, err = dockerruntime.NewSDKClient(a.config.DockerHost)
		if err != nil {
			return fmt.Errorf("create Docker Engine client: %w", err)
		}

	case rlarkv1alpha1.AgentTypeRaw:
		if a.config.RawSupervisorSocket == "" {
			return fmt.Errorf("raw-supervisor socket is required for Raw agent")
		}

	default:
		return fmt.Errorf("unknown AgentType: %s", a.config.AgentType)
	}

	// Initialize local listener and dialer
	a.localListener, a.localDialer = utils.NetPipeWithBuffer(65536)

	return nil
}

func validateTaskRuntimeResyncPeriod(mode string, period time.Duration) error {
	if mode != "node" && period < 0 {
		return fmt.Errorf("task Runtime resync period must not be negative")
	}
	return nil
}

// Run runs the component.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.init(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	a.drainMu.Lock()
	a.drain = cancel
	a.drainMu.Unlock()
	defer func() {
		a.ready.Store(false)
		a.drainMu.Lock()
		a.drain = nil
		a.drainMu.Unlock()
		cancel()
	}()

	if a.config.AgentType == string(rlarkv1alpha1.AgentTypeRaw) {
		return (&clusterAgent{a: a}).Run(ctx)
	}

	var eg errgroup.Group

	// Run tunnel for all modes
	eg.Go(func() error {
		role := ""
		if a.config.Mode == "node" {
			role = "node-agent:" + common.NodeName("-")
		} else if a.config.AgentType == string(rlarkv1alpha1.AgentTypeDocker) {
			// Docker cluster agents need both the cluster heartbeat and a
			// node-scoped heartbeat so the shared Node controller can schedule
			// the host. The server recognizes this role specially.
			role = "docker-agent:" + common.NodeName(a.config.NodeName)
		}
		return a.runTunnel(ctx, role)
	})

	// Run local HTTP server for all modes
	eg.Go(func() error {
		return a.runLocalHTTPServer(ctx)
	})

	// Run cluster agent (controller manager) based on mode
	if a.config.Mode == "cluster" || a.config.Mode == "both" {
		eg.Go(func() error {
			return (&clusterAgent{a: a}).Run(ctx)
		})
	}

	// Run node agent based on mode
	if a.config.Mode == "node" || a.config.Mode == "both" {
		eg.Go(func() error {
			return (&nodeAgent{a: a}).Run(ctx)
		})
	}

	if err := eg.Wait(); err != nil {
		return fmt.Errorf("agent run error: %w", err)
	}
	return nil
}
