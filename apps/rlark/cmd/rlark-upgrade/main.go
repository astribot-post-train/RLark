// rlark-upgrade is a fully autonomous upgrade tool.
// It performs upgrade with automatic health check and rollback.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rlinf/rlark/apps/rlark/astribot/clusterupgrade"
	"github.com/rlinf/rlark/apps/rlark/pkg/log"
	"github.com/rlinf/rlark/apps/rlark/pkg/upgrade"
	"github.com/rlinf/rlark/apps/rlark/pkg/utils"
)

const (
	// Exit codes
	ExitSuccess             = 0
	ExitUpgradeFailed       = 1
	ExitRolledBack          = 2
	ExitRollbackFailed      = 3
	ExitPrecheckFailed      = 4
	ExitHealthCheckFailed   = 5
	ExitExecutorInterrupted = 143
)

func main() {
	// Setup signal handling for graceful shutdown
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Initialize logger
	log.InitLogger("info")
	logger := log.GetLogger()

	logger.Info("starting rlark-upgrade")

	if brokerConfig := os.Getenv("RLARK_NODE_IMAGE_BROKER_CONFIG"); brokerConfig != "" {
		if err := clusterupgrade.ServeNodeImageBroker(ctx, brokerConfig); err != nil {
			logger.Error(err, "node image broker stopped")
			os.Exit(ExitPrecheckFailed)
		}
		return
	}

	mode := os.Getenv("RLARK_UPGRADE_MODE")
	if mode != "" && mode != clusterupgrade.DriverStock {
		if mode != clusterupgrade.ClusterOnlyMode && mode != clusterupgrade.ManagedNodeMode {
			logger.Error(fmt.Errorf("unrecognized upgrade execution mode"), "invalid configuration")
			os.Exit(ExitPrecheckFailed)
		}
		data, err := os.ReadFile(utils.GetEnv("DEPLOY_CONFIG", "/etc/rlark/deploy-config.yaml"))
		if err != nil {
			logger.Error(err, "read image-only upgrade config")
			os.Exit(ExitPrecheckFailed)
		}
		err = clusterupgrade.RunInCluster(ctx, clusterupgrade.JobRequest{
			JobUID: os.Getenv("RLARK_UPGRADE_JOB_UID"), PodName: os.Getenv("POD_NAME"), PodUID: os.Getenv("POD_UID"),
			Mode: mode, Cluster: os.Getenv("RLARK_UPGRADE_CLUSTER"), ChildName: os.Getenv("RLARK_UPGRADE_CHILD_NAME"),
			Deployment: os.Getenv("RLARK_UPGRADE_DEPLOYMENT"), DeployConfig: string(data),
			AutoRollback: utils.GetEnvBool("AUTO_ROLLBACK", true), HealthTimeout: utils.GetEnvDuration("HEALTH_CHECK_TIMEOUT", 5*time.Minute),
		})
		if err == nil {
			os.Exit(ExitSuccess)
		}
		logger.Error(err, "image-only upgrade completed with an error")
		clusterupgrade.WriteJobDiagnostic(err)
		switch {
		case ctx.Err() != nil || errors.Is(err, clusterupgrade.ErrExecutorInterrupted) || errors.Is(err, clusterupgrade.ErrLeaseLost):
			os.Exit(ExitExecutorInterrupted)
		case errors.Is(err, clusterupgrade.ErrRolledBack):
			os.Exit(ExitRolledBack)
		case errors.Is(err, clusterupgrade.ErrRollbackFailed):
			os.Exit(ExitRollbackFailed)
		default:
			os.Exit(ExitUpgradeFailed)
		}
	}

	os.Exit(runStock(ctx, mode))
}

// A separate call lets the Lease cleanup run before main exits.
func runStock(ctx context.Context, mode string) int {
	logger := log.GetLogger()
	// Load configuration
	config := &upgrade.Config{
		StockDataPlane:      mode == clusterupgrade.DriverStock,
		DeployConfigPath:    utils.GetEnv("DEPLOY_CONFIG", "/etc/rlark/deploy-config.yaml"),
		RollbackConfigPath:  utils.GetEnv("ROLLBACK_CONFIG", "/etc/rlark/rollback-deploy-config.yaml"),
		AutoRollback:        utils.GetEnvBool("AUTO_ROLLBACK", true),
		HealthCheckTimeout:  utils.GetEnvDuration("HEALTH_CHECK_TIMEOUT", 5*time.Minute),
		HealthCheckInterval: utils.GetEnvDuration("HEALTH_CHECK_INTERVAL", 10*time.Second),
	}

	if config.DeployConfigPath == "" {
		logger.Error(fmt.Errorf("DEPLOY_CONFIG is required"), "invalid config")
		return ExitPrecheckFailed
	}

	if config.StockDataPlane && os.Getenv("RLARK_UPGRADE_STOCK_LOCK") == "1" {
		return runLockedStock(ctx, config)
	}

	logger.Info("loaded config",
		"deployConfig", config.DeployConfigPath,
		"rollbackConfig", config.RollbackConfigPath,
		"autoRollback", config.AutoRollback,
	)

	// Create upgrader
	upgrader, err := upgrade.NewUpgrader(config)
	if err != nil {
		logger.Error(err, "create upgrader")
		return ExitPrecheckFailed
	}

	// Perform upgrade (includes multi-stage health check)
	err = upgrader.Upgrade(ctx)
	if err == nil {
		logger.Info("upgrade completed successfully")
		return ExitSuccess
	}

	logger.Error(err, "upgrade failed")

	// Handle rollback
	if !config.AutoRollback {
		logger.Info("auto-rollback disabled, exiting")
		return ExitUpgradeFailed
	}

	logger.Info("starting automatic rollback")

	rollbackErr := upgrader.Rollback(ctx)
	if rollbackErr != nil {
		logger.Error(rollbackErr, "rollback failed")
		return ExitRollbackFailed
	}

	logger.Info("rollback completed successfully")
	return ExitRolledBack
}

func runLockedStock(ctx context.Context, config *upgrade.Config) int {
	logger := log.GetLogger()
	fromState := os.Getenv("RLARK_UPGRADE_STOCK_CONFIG_FROM_STATE") == "1"
	var requested []byte
	if !fromState {
		var err error
		requested, err = os.ReadFile(config.DeployConfigPath)
		if err != nil {
			logger.Error(err, "read locked stock deploy config")
			return ExitPrecheckFailed
		}
	}
	stock, err := clusterupgrade.HoldStockInCluster(ctx, clusterupgrade.ExecutorIdentity{
		ChildUID: os.Getenv("RLARK_UPGRADE_JOB_UID"), PodName: os.Getenv("POD_NAME"), PodUID: os.Getenv("POD_UID"),
	})
	if err != nil {
		logger.Error(err, "acquire stock upgrade lease")
		clusterupgrade.WriteJobDiagnostic(err)
		if ctx.Err() != nil {
			return ExitExecutorInterrupted
		}
		return ExitPrecheckFailed
	}
	err = stock.Run(string(requested), fromState, config.AutoRollback, func(effective, baseline string) (clusterupgrade.StockOperations, error) {
		config.StockDeployConfig = &effective
		config.StockRollbackConfig = &baseline
		config.CheckStockWrite = stock.Check
		return upgrade.NewUpgrader(config)
	})
	if ctx.Err() != nil || errors.Is(err, clusterupgrade.ErrExecutorInterrupted) || errors.Is(err, clusterupgrade.ErrLeaseLost) {
		stock.RetainForRecovery()
		return ExitExecutorInterrupted
	}
	stock.Release()
	if err == nil {
		logger.Info("stock upgrade completed successfully")
		return ExitSuccess
	}
	logger.Error(err, "stock upgrade completed with an error")
	clusterupgrade.WriteJobDiagnostic(err)
	switch {
	case errors.Is(err, clusterupgrade.ErrRolledBack):
		return ExitRolledBack
	case errors.Is(err, clusterupgrade.ErrRollbackFailed):
		return ExitRollbackFailed
	default:
		return ExitUpgradeFailed
	}
}
