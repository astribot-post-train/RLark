package agent

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/addon"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/base"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/delivery"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/node"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/pod"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task"
	dockerruntime "github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/runtime/docker"
	dockerray "github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/workload/ray/docker"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/workload"
	"github.com/rlinf/rlark/apps/rlark/pkg/auth/cert"
	"github.com/rlinf/rlark/apps/rlark/pkg/common"
	"github.com/rlinf/rlark/apps/rlark/pkg/log"
	"github.com/rlinf/rlark/apps/rlark/pkg/rawnode"
	"github.com/rlinf/rlark/apps/rlark/pkg/server"
	"github.com/rlinf/rlark/apps/rlark/pkg/tenant"
)

// clusterAgent manages cluster-level operations (controller manager).
type clusterAgent struct {
	a *Agent
}

// Run runs the component.
func (c *clusterAgent) Run(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("clusterAgent")
	ctrl.SetLogger(logger)

	agentType := c.a.config.AgentType
	clusterID := c.a.config.ClientConfig.ServerNamespace
	logger.Info(fmt.Sprintf("agentType=%s, clusterID=%s", agentType, clusterID))
	if clusterID == "" {
		return fmt.Errorf("cluster ID is empty")
	}

	if agentType == string(rlarkv1alpha1.AgentTypeRaw) {
		return c.runRaw(ctx)
	}

	mm, err := ctrl.NewManager(c.a.managementConfig, ctrl.Options{
		Scheme:  controllers.MgmtScheme,
		Metrics: metricsserver.Options{BindAddress: c.a.config.MetricsBindAddress},
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				clusterID: {},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("create management manager: %w", err)
	}
	mclient, err := client.New(c.a.managementConfig, client.Options{Scheme: controllers.MgmtScheme})
	if err != nil {
		return fmt.Errorf("create management direct client: %w", err)
	}

	bc := base.Controller{
		ManagementClient:    mclient,
		ManagementNamespace: clusterID,
		AgentType:           agentType,
		Image:               c.a.config.Image,
	}
	if rlarkv1alpha1.AgentType(agentType) == rlarkv1alpha1.AgentTypeDocker {
		return c.runDocker(ctx, mm, mclient, bc, clusterID)
	}

	var lm interface {
		Start(ctx context.Context) error
		Add(manager.Runnable) error
	}
	var localManager ctrl.Manager

	switch rlarkv1alpha1.AgentType(agentType) {
	case rlarkv1alpha1.AgentTypeKubernetes:
		if c.a.localKubeConfig == nil {
			return fmt.Errorf("local kube config is required for kubernetes agent")
		}
		m, err := ctrl.NewManager(c.a.localKubeConfig, ctrl.Options{
			Scheme:  controllers.MgmtScheme,
			Metrics: metricsserver.Options{BindAddress: "0"},
		})
		if err != nil {
			return fmt.Errorf("create local manager: %w", err)
		}
		lclient, err := client.New(c.a.localKubeConfig, client.Options{Scheme: controllers.MgmtScheme})
		if err != nil {
			return fmt.Errorf("create local direct client: %w", err)
		}
		transport, err := rest.TransportFor(c.a.localKubeConfig)
		if err != nil {
			return fmt.Errorf("create local Kubernetes HTTP transport: %w", err)
		}
		bc.LocalKubeClient = lclient
		bc.LocalKubeCachedClient = m.GetClient()
		bc.LocalKubeHTTP = &http.Client{
			Transport: transport,
			Timeout:   10 * time.Second,
		}
		bc.LocalKubeAPIHost = c.a.localKubeConfig.Host
		bc.LocalKubeConfig = c.a.localKubeConfig
		lm = m
		localManager = m

	case rlarkv1alpha1.AgentTypeDocker:
		// TODO: initialize Docker controller manager
		return fmt.Errorf("docker agent is not implemented yet")

	default:
		return fmt.Errorf("unknown AgentType: %s", agentType)
	}

	// Setup Task controllers
	taskController := bc
	taskController.PullMaxConcurrentReconciles = c.a.config.ControllerConcurrency.TaskPull
	taskController.PushMaxConcurrentReconciles = map[string]int{
		"task-deployment":  c.a.config.ControllerConcurrency.TaskDeployment,
		"task-daemonset":   c.a.config.ControllerConcurrency.TaskDaemonSet,
		"task-statefulset": c.a.config.ControllerConcurrency.TaskStatefulSet,
	}
	tc := task.NewTaskController(taskController)

	execution, err := task.NewKubernetesExecutionCoordinator(taskController.LocalKubeClient, taskController.Image)
	if err != nil {
		return fmt.Errorf("initialize task execution coordinator: %w", err)
	}
	tc.WithExecutionCoordinator(execution)
	taskHost, err := task.NewKubernetesHost(tc, execution)
	if err != nil {
		return fmt.Errorf("initialize Task Host: %w", err)
	}
	tc.WithHost(taskHost)
	if err := tc.SetupPullController(mm); err != nil {
		return fmt.Errorf("setup task pull controller: %w", err)
	}
	if err := mm.Add(task.NewTaskResync(mclient, clusterID, c.a.config.TaskRuntimeResyncPeriod, tc)); err != nil {
		return fmt.Errorf("setup Task Runtime resync: %w", err)
	}
	if err := tc.SetupPushController(lm); err != nil {
		return fmt.Errorf("setup task push controller: %w", err)
	}

	// Setup Node controllers
	nodeController := bc
	nodeController.PushMaxConcurrentReconciles = map[string]int{
		"node-k8snode": c.a.config.ControllerConcurrency.NodePush,
	}
	nc := node.NewNodeController(nodeController)
	if err := lm.Add(node.NewOfflineSweeper(nc, 0)); err != nil {
		return fmt.Errorf("setup node offline sweeper: %w", err)
	}
	if err := node.IndexLocalFields(ctx, localManager); err != nil {
		return fmt.Errorf("index local fields for node controller: %w", err)
	}
	if err := nc.SetupPullController(mm); err != nil {
		return fmt.Errorf("setup node pull controller: %w", err)
	}
	if err := nc.SetupPushController(lm); err != nil {
		return fmt.Errorf("setup node push controller: %w", err)
	}

	// Setup Pod controllers (push-only: reports local K8s Pods to management Pod CRs)
	podController := bc
	podController.PushMaxConcurrentReconciles = map[string]int{
		"pod-k8spod": c.a.config.ControllerConcurrency.PodPush,
	}
	pc := pod.NewPodController(podController)
	if err := lm.Add(pod.NewOrphanSweeper(pc, c.a.config.PodOrphanSweepInterval, c.a.config.PodOrphanSweepPageSize, c.a.config.PodStaleTTL)); err != nil {
		return fmt.Errorf("setup pod orphan sweeper: %w", err)
	}
	if err := pc.SetupPullController(mm); err != nil {
		return fmt.Errorf("setup pod pull controller: %w", err)
	}
	if err := pc.SetupPushController(lm); err != nil {
		return fmt.Errorf("setup pod push controller: %w", err)
	}

	// Setup Addon controllers (pull-only: watches management Addon CRs and deploys to local cluster)
	addonController := bc
	addonController.PullMaxConcurrentReconciles = c.a.config.ControllerConcurrency.AddonPull
	ac := addon.NewAddonController(addonController)
	if err := ac.SetupPullController(mm); err != nil {
		return fmt.Errorf("setup addon pull controller: %w", err)
	}
	if err := ac.SetupPushController(lm); err != nil {
		return fmt.Errorf("setup addon push controller: %w", err)
	}

	deliveryReconciler, err := delivery.New(c.a.localKubeConfig, mclient, bc.LocalKubeClient, clusterID)
	if err != nil {
		return fmt.Errorf("create delivery controller: %w", err)
	}
	if err := deliveryReconciler.Setup(mm, localManager); err != nil {
		return fmt.Errorf("setup delivery controller: %w", err)
	}

	var eg errgroup.Group
	eg.Go(func() error { return mm.Start(ctx) })
	eg.Go(func() error { return lm.Start(ctx) })
	return eg.Wait()
}

func (c *clusterAgent) runDocker(ctx context.Context, mm ctrl.Manager, mclient client.Client, bc base.Controller, clusterID string) error {
	logger := log.FromContext(ctx).WithName("docker-agent").WithValues("cluster", clusterID)
	logger.Info("initializing Docker runtime")
	if c.a.localDockerClient == nil {
		return fmt.Errorf("Docker Engine client is not configured")
	}
	nodeName := c.a.config.NodeName
	if nodeName == "" {
		nodeName = common.Hostname("docker-node")
	}
	logger.Info("ensuring Docker Node", "node", nodeName)
	node, err := ensureDockerNode(ctx, mclient, clusterID, nodeName)
	if err != nil {
		logger.Error(err, "Docker Node setup failed", "node", nodeName)
		return err
	}
	logger.Info("Docker Node ready", "node", node.Name, "uid", node.UID)
	info, err := c.a.localDockerClient.Info(ctx)
	if err != nil {
		logger.Error(err, "Docker Engine inspection failed")
		return fmt.Errorf("inspect Docker Engine identity: %w", err)
	}
	incarnation := info.ID
	if incarnation == "" {
		incarnation = nodeName
	}
	logger.Info("Docker Engine ready", "incarnation", incarnation)
	provider := &dockerruntime.Provider{ID: dockerruntime.DefaultID, Client: c.a.localDockerClient}
	admission := dockerruntime.NodeAdmission{Client: mclient, Node: client.ObjectKeyFromObject(node), NodeUID: string(node.UID), Incarnation: incarnation, TenantID: tenant.IDOf(node)}
	provider.Admission = admission
	bc.LocalDockerClient = c.a.localDockerClient
	taskController := bc
	taskController.PullMaxConcurrentReconciles = c.a.config.ControllerConcurrency.TaskPull
	tc := task.NewTaskController(taskController)
	execution, err := dockerruntime.NewCoordinator(ctx, provider, []workload.Adapter{dockerray.Adapter{}})
	if err != nil {
		return fmt.Errorf("initialize Docker task execution coordinator: %w", err)
	}
	logger.Info("Docker task coordinator ready")
	tc.WithExecutionCoordinator(execution)
	taskHost, err := dockerruntime.NewHost(ctx, mclient, admission, provider, []workload.Adapter{dockerray.Adapter{}})
	if err != nil {
		return fmt.Errorf("initialize Docker Task Host: %w", err)
	}
	logger.Info("Docker Task host ready")
	tc.WithHost(taskHost)
	if err := tc.SetupPullController(mm); err != nil {
		return fmt.Errorf("setup Docker Task pull controller: %w", err)
	}
	logger.Info("Docker Task pull controller ready")
	if err := mm.Add(task.NewTaskResync(mclient, clusterID, c.a.config.TaskRuntimeResyncPeriod, tc)); err != nil {
		return fmt.Errorf("setup Docker Task resync: %w", err)
	}
	logger.Info("Docker Task resync ready")
	local := &dockerManager{}
	if err := tc.SetupPushController(local); err != nil {
		return fmt.Errorf("setup Docker Task push controller: %w", err)
	}
	logger.Info("Docker Task push controller ready")
	observer := &dockerruntime.NodeObserver{Client: mclient, Engine: c.a.localDockerClient, Node: client.ObjectKeyFromObject(node), NodeIP: c.a.config.DockerNodeIP, NodeUID: string(node.UID), Incarnation: incarnation}
	if err := local.Add(observer); err != nil {
		return fmt.Errorf("setup Docker Node observer: %w", err)
	}
	logger.Info("Docker Node observer ready")
	eg, ctx := errgroup.WithContext(ctx)
	logger.Info("starting Docker managers")
	eg.Go(func() error { return mm.Start(ctx) })
	eg.Go(func() error { return local.Start(ctx) })
	return eg.Wait()
}

func ensureDockerNode(ctx context.Context, management client.Client, namespace, name string) (*rlarkv1alpha1.Node, error) {
	var node rlarkv1alpha1.Node
	key := client.ObjectKey{Namespace: namespace, Name: name}
	if err := management.Get(ctx, key, &node); err == nil {
		if node.Spec.AgentType != "" && node.Spec.AgentType != rlarkv1alpha1.AgentTypeDocker {
			return nil, fmt.Errorf("Node %s is owned by agent type %s", key.String(), node.Spec.AgentType)
		}
		return &node, nil
	} else if client.IgnoreNotFound(err) != nil {
		return nil, fmt.Errorf("read Docker Node: %w", err)
	}
	node = rlarkv1alpha1.Node{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}, Spec: rlarkv1alpha1.NodeSpec{AgentType: rlarkv1alpha1.AgentTypeDocker}}
	tenant.SetIdentity(&node, tenant.DefaultID, "rlark-agent")
	if err := management.Create(ctx, &node); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if getErr := management.Get(ctx, key, &node); getErr == nil {
				return &node, nil
			}
		}
		return nil, fmt.Errorf("create Docker Node: %w", err)
	}
	return &node, nil
}

func rawIdentityFromClientCertificate(config server.ClientConfig) (rawnode.Identity, error) {
	certPEM, err := os.ReadFile(config.ClientCertPath)
	if err != nil {
		return rawnode.Identity{}, fmt.Errorf("read client certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(config.ClientKeyPath)
	if err != nil {
		return rawnode.Identity{}, fmt.Errorf("read client key: %w", err)
	}
	data, err := cert.LoadData(certPEM, keyPEM)
	if err != nil {
		return rawnode.Identity{}, err
	}
	metadata, ok := cert.GetX509CertMeta(data.Cert)
	if !ok {
		return rawnode.Identity{}, fmt.Errorf("parse client certificate metadata")
	}
	return rawnode.FromMetadata(metadata)
}

func runRawHeartbeat(ctx context.Context, client *server.Client) {
	if client == nil {
		return
	}
	beat := func() {
		requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		response, err := client.DoRequest(requestCtx, http.MethodPost, client.BuildURL("api", "raw", "heartbeat"), nil)
		if response != nil {
			_ = response.Body.Close()
		}
		if err != nil || response == nil || response.StatusCode < 200 || response.StatusCode >= 300 {
			return
		}
	}
	beat()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			beat()
		}
	}
}
