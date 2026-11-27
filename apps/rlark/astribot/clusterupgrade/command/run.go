// Package command dispatches Astribot's dedicated upgrade executor modes.
package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/rlinf/rlark/apps/rlark/astribot/clusterupgrade"
	"github.com/rlinf/rlark/apps/rlark/pkg/log"
	"github.com/rlinf/rlark/apps/rlark/pkg/upgrade"
	"github.com/rlinf/rlark/apps/rlark/pkg/utils"
)

const (
	ExitSuccess             = 0
	ExitUpgradeFailed       = 1
	ExitRolledBack          = 2
	ExitRollbackFailed      = 3
	ExitPrecheckFailed      = 4
	ExitExecutorInterrupted = 143
)

// Run handles an Astribot execution mode and returns whether it selected one.
func Run(ctx context.Context, mode string) (bool, int) {
	logger := log.GetLogger()
	if brokerConfig := os.Getenv("RLARK_NODE_IMAGE_BROKER_CONFIG"); brokerConfig != "" {
		if err := clusterupgrade.ServeNodeImageBroker(ctx, brokerConfig); err != nil {
			logger.Error(err, "node image broker stopped")
			return true, ExitPrecheckFailed
		}
		return true, ExitSuccess
	}
	if mode != "" && mode != clusterupgrade.DriverStock {
		if mode != clusterupgrade.ClusterOnlyMode && mode != clusterupgrade.ManagedNodeMode {
			logger.Error(fmt.Errorf("unrecognized upgrade execution mode"), "invalid configuration")
			return true, ExitPrecheckFailed
		}
		data, err := os.ReadFile(utils.GetEnv("DEPLOY_CONFIG", "/etc/rlark/deploy-config.yaml"))
		if err != nil {
			logger.Error(err, "read image-only upgrade config")
			return true, ExitPrecheckFailed
		}
		err = clusterupgrade.RunInCluster(ctx, clusterupgrade.JobRequest{
			JobUID: os.Getenv("RLARK_UPGRADE_JOB_UID"), PodName: os.Getenv("POD_NAME"), PodUID: os.Getenv("POD_UID"),
			Mode: mode, Cluster: os.Getenv("RLARK_UPGRADE_CLUSTER"), ChildName: os.Getenv("RLARK_UPGRADE_CHILD_NAME"),
			Deployment: os.Getenv("RLARK_UPGRADE_DEPLOYMENT"), DeployConfig: string(data),
			AutoRollback: utils.GetEnvBool("AUTO_ROLLBACK", true), HealthTimeout: utils.GetEnvDuration("HEALTH_CHECK_TIMEOUT", 5*time.Minute),
		})
		if err == nil {
			return true, ExitSuccess
		}
		logger.Error(err, "image-only upgrade completed with an error")
		clusterupgrade.WriteJobDiagnostic(err)
		switch {
		case ctx.Err() != nil || errors.Is(err, clusterupgrade.ErrExecutorInterrupted) || errors.Is(err, clusterupgrade.ErrLeaseLost):
			return true, ExitExecutorInterrupted
		case errors.Is(err, clusterupgrade.ErrRolledBack):
			return true, ExitRolledBack
		case errors.Is(err, clusterupgrade.ErrRollbackFailed):
			return true, ExitRollbackFailed
		default:
			return true, ExitUpgradeFailed
		}
	}

	if mode == clusterupgrade.DriverStock && os.Getenv("RLARK_UPGRADE_STOCK_LOCK") == "1" {
		return true, runLockedStock(ctx, &upgrade.Config{
			StockDataPlane:      true,
			DeployConfigPath:    utils.GetEnv("DEPLOY_CONFIG", "/etc/rlark/deploy-config.yaml"),
			AutoRollback:        utils.GetEnvBool("AUTO_ROLLBACK", true),
			HealthCheckTimeout:  utils.GetEnvDuration("HEALTH_CHECK_TIMEOUT", 5*time.Minute),
			HealthCheckInterval: utils.GetEnvDuration("HEALTH_CHECK_INTERVAL", 10*time.Second),
		})
	}
	return false, 0
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
		if ctx.Err() != nil || errors.Is(err, clusterupgrade.ErrExecutorInterrupted) {
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
