package controllermanager

import (
	"context"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/archive"
	"github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/cluster"
	"github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/domain"
	"github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/job"
	"github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/node"
	"github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/replication"
	"github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/sync"
	"github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/task"
	"github.com/rlinf/rlark/apps/rlark/pkg/controllermanager/workflow"
	"github.com/rlinf/rlark/apps/rlark/pkg/db"
	"github.com/rlinf/rlark/apps/rlark/pkg/log"
	archivestore "github.com/rlinf/rlark/apps/rlark/pkg/persistence"
)

// Reconciler reconciles resources.
type Reconciler interface {
	SetupWithManager(ctrl.Manager) error
}

// Extension is an explicitly assembled controller extension. Native binaries
// remain independent from optional API groups and their implementation modules.
type Extension struct {
	AddToScheme func(*runtime.Scheme) error
	Setup       func(manager.Manager) error
	// JobOwner receives an uncached reader for authoritative ownership checks.
	JobOwner func(client.Reader) job.OwnerResolver
}

// New creates a new instance.
func New(config Config, extensions ...Extension) (manager.Manager, error) {
	localScheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(localScheme); err != nil {
		return nil, err
	}
	if err := rlarkv1alpha1.AddToScheme(localScheme); err != nil {
		return nil, err
	}
	for _, extension := range extensions {
		if extension.AddToScheme != nil {
			if err := extension.AddToScheme(localScheme); err != nil {
				return nil, fmt.Errorf("register extension API: %w", err)
			}
		}
	}
	scheme := localScheme

	logger := log.GetLogger()
	ctrl.SetLogger(logger)
	if err := config.ControllerConcurrency.Validate(); err != nil {
		return nil, fmt.Errorf("validate controller concurrency: %w", err)
	}

	restConfig, err := config.KubeClientConfig.BuildRestConfig()
	if err != nil {
		return nil, fmt.Errorf("build Kubernetes client config: %w", err)
	}
	options, err := managerOptions(config, restConfig, scheme)
	if err != nil {
		return nil, err
	}
	mgr, err := ctrl.NewManager(restConfig, options)
	if err != nil {
		return nil, fmt.Errorf("create manager: %w", err)
	}
	replicationReconciler, err := replication.New(restConfig, mgr.GetClient())
	if err != nil {
		return nil, fmt.Errorf("create replication controller: %w", err)
	}

	jobController := &job.Reconciler{Client: mgr.GetClient(), Scheme: scheme, MaxConcurrentReconciles: config.ControllerConcurrency.Job}
	var owners []job.OwnerResolver
	for _, extension := range extensions {
		if extension.JobOwner != nil {
			owner := extension.JobOwner(mgr.GetAPIReader())
			if owner == nil {
				return nil, fmt.Errorf("extension Job owner constructor returned nil")
			}
			owners = append(owners, owner)
		}
	}
	if len(owners) > 0 {
		jobController.ExternalOwner = extensionOwners(owners)
	}
	reconcilers := []Reconciler{
		replicationReconciler,
		jobController,
		&task.Reconciler{
			Client:                  mgr.GetClient(),
			Scheme:                  scheme,
			MaxConcurrentReconciles: config.ControllerConcurrency.Task,
		},
		&workflow.Reconciler{
			Client:                  mgr.GetClient(),
			Scheme:                  scheme,
			MaxConcurrentReconciles: config.ControllerConcurrency.Workflow,
		},
		&node.Reconciler{
			Client:                  mgr.GetClient(),
			Scheme:                  scheme,
			MaxConcurrentReconciles: config.ControllerConcurrency.Node,
		},
		&cluster.Reconciler{
			Client:                  mgr.GetClient(),
			Scheme:                  scheme,
			MaxConcurrentReconciles: config.ControllerConcurrency.Cluster,
		},
		&domain.Reconciler{
			Client:                  mgr.GetClient(),
			Scheme:                  scheme,
			MaxConcurrentReconciles: config.ControllerConcurrency.Domain,

			KubeClientConfig: config.KubeClientConfig,
			ServerAddress:    config.ServerAddress,
		},
	}

	// 初始化持久化后端：优先 PG，否则用 Secret（测试模式）
	var backend archivestore.Backend

	if config.DBConfigPath != "" {
		dbConfig := db.DefaultConfig()
		data, err := os.ReadFile(config.DBConfigPath)
		if err != nil {
			return nil, fmt.Errorf("read database config file: %w", err)
		}
		if err := db.UnmarshalConfig(data, &dbConfig); err != nil {
			return nil, fmt.Errorf("unmarshal database config: %w", err)
		}
		database, err := db.Open(dbConfig)
		if err != nil {
			return nil, fmt.Errorf("open database: %w", err)
		}
		ctx := context.Background()
		if err := database.Migrate(ctx); err != nil {
			return nil, fmt.Errorf("run database migrations: %w", err)
		}
		logger.Info("database connected and migrated")
		backend = archivestore.NewPostgresBackend(database.DB)
		if err := mgr.Add(&sync.OrphanReconciler{Reader: mgr.GetAPIReader(), DB: database.DB}); err != nil {
			return nil, fmt.Errorf("setup orphan deletion reconciliation: %w", err)
		}
	} else if config.EnableSecretBackend {
		// Secret Backend：测试/CI 模式，用 K8s Secret 存归档快照。
		backend = archivestore.NewSecretBackend(mgr.GetClient(), config.KubeClientConfig.DefaultNamespace())
		logger.Info("using Secret backend for persistence (test mode)")
	} else {
		logger.Error(nil, "RLark controller manager is running without persistent storage.")
	}

	// 启动 Syncer（如果配置了持久化后端）
	if backend != nil {
		reconcilers = append(reconcilers,
			sync.NewJobReconciler(config.ControllerConcurrency.JobSync, mgr.GetClient(), backend),
			sync.NewTaskReconciler(config.ControllerConcurrency.TaskSync, mgr.GetClient(), backend),
			sync.NewWorkflowReconciler(config.ControllerConcurrency.WorkflowSync, mgr.GetClient(), backend),
			sync.NewNodeReconciler(config.ControllerConcurrency.NodeSync, mgr.GetClient(), backend),
		)

		for _, res := range archive.DefaultRegistry.Resources() {
			reconcilers = append(reconcilers, archive.NewReconciler(mgr.GetClient(), scheme, res,
				config.ControllerConcurrency.Archive, config.ArchiveTTL),
			)
		}
	}

	for _, extension := range extensions {
		if extension.Setup != nil {
			if err := extension.Setup(mgr); err != nil {
				return nil, fmt.Errorf("setup controller extension: %w", err)
			}
		}
	}
	for _, r := range reconcilers {
		if err := r.SetupWithManager(mgr); err != nil {
			return nil, fmt.Errorf("setup controller: %w", err)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("setup health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("setup ready check: %w", err)
	}

	return mgr, nil
}

func managerOptions(config Config, restConfig *rest.Config, schemes ...*runtime.Scheme) (ctrl.Options, error) {
	localScheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(localScheme))
	utilruntime.Must(rlarkv1alpha1.AddToScheme(localScheme))
	if len(schemes) > 0 && schemes[0] != nil {
		localScheme = schemes[0]
	}
	options := ctrl.Options{
		Scheme:                 localScheme,
		Metrics:                metricsserver.Options{BindAddress: config.MetricsBindAddress},
		HealthProbeBindAddress: config.ProbeBindAddress,
		LeaderElection:         config.LeaderElection.Enabled,
	}
	if !config.LeaderElection.Enabled {
		return options, nil
	}

	leNamespace, leName, err := config.LeaderElection.NamespaceAndName(config.KubeClientConfig.DefaultNamespace())
	if err != nil {
		return ctrl.Options{}, fmt.Errorf("resolve leader election key: %w", err)
	}
	if err := config.LeaderElection.Validate(config.KubeClientConfig.DefaultNamespace()); err != nil {
		return ctrl.Options{}, fmt.Errorf("validate leader election config: %w", err)
	}
	options.LeaderElectionID = leName
	options.LeaderElectionNamespace = leNamespace
	if config.LeaderElection.Identity != "" {
		clientset, err := kubernetes.NewForConfig(restConfig)
		if err != nil {
			return ctrl.Options{}, fmt.Errorf("create leader election client: %w", err)
		}
		lock, err := config.LeaderElection.ResourceLock(clientset, config.KubeClientConfig.DefaultNamespace())
		if err != nil {
			return ctrl.Options{}, fmt.Errorf("create leader election lock: %w", err)
		}
		options.LeaderElectionResourceLockInterface = lock
	}
	if config.LeaderElection.LeaseDuration > 0 {
		options.LeaseDuration = &config.LeaderElection.LeaseDuration
		options.RenewDeadline = &config.LeaderElection.RenewDeadline
		options.RetryPeriod = &config.LeaderElection.RetryPeriod
	}
	return options, nil
}

// extensionOwners rejects ambiguous claims instead of letting registration order
// decide which controller owns a workload.
type extensionOwners []job.OwnerResolver

func (owners extensionOwners) Owns(ctx context.Context, value *rlarkv1alpha1.Job) (bool, error) {
	claimed := false
	for _, owner := range owners {
		owned, err := owner.Owns(ctx, value)
		if err != nil {
			return false, err
		}
		if owned && claimed {
			return false, fmt.Errorf("multiple extensions claim Job %s/%s", value.Namespace, value.Name)
		}
		claimed = claimed || owned
	}
	return claimed, nil
}
