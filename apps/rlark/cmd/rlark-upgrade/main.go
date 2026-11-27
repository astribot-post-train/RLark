// rlark-upgrade is a fully autonomous upgrade tool.
// It performs upgrade with automatic health check and rollback.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rlinf/rlark/apps/rlark/astribot/clusterupgrade/command"
	"github.com/rlinf/rlark/apps/rlark/pkg/log"
	"github.com/rlinf/rlark/apps/rlark/pkg/upgrade"
	"github.com/rlinf/rlark/apps/rlark/pkg/utils"
)

func main() {
	// Setup signal handling for graceful shutdown
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Initialize logger
	log.InitLogger("info")
	logger := log.GetLogger()

	logger.Info("starting rlark-upgrade")

	mode := os.Getenv("RLARK_UPGRADE_MODE")
	if handled, code := command.Run(ctx, mode); handled {
		os.Exit(code)
	}
	os.Exit(runStock(ctx, mode))
}

// A separate call lets the Lease cleanup run before main exits.
func runStock(ctx context.Context, mode string) int {
	logger := log.GetLogger()
	// Load configuration
	config := &upgrade.Config{
		StockDataPlane:      mode == "stock",
		DeployConfigPath:    utils.GetEnv("DEPLOY_CONFIG", "/etc/rlark/deploy-config.yaml"),
		RollbackConfigPath:  utils.GetEnv("ROLLBACK_CONFIG", "/etc/rlark/rollback-deploy-config.yaml"),
		AutoRollback:        utils.GetEnvBool("AUTO_ROLLBACK", true),
		HealthCheckTimeout:  utils.GetEnvDuration("HEALTH_CHECK_TIMEOUT", 5*time.Minute),
		HealthCheckInterval: utils.GetEnvDuration("HEALTH_CHECK_INTERVAL", 10*time.Second),
	}

	if config.DeployConfigPath == "" {
		logger.Error(fmt.Errorf("DEPLOY_CONFIG is required"), "invalid config")
		return command.ExitPrecheckFailed
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
		return command.ExitPrecheckFailed
	}

	// Perform upgrade (includes multi-stage health check)
	err = upgrader.Upgrade(ctx)
	if err == nil {
		logger.Info("upgrade completed successfully")
		return command.ExitSuccess
	}

	logger.Error(err, "upgrade failed")

	// Handle rollback
	if !config.AutoRollback {
		logger.Info("auto-rollback disabled, exiting")
		return command.ExitUpgradeFailed
	}

	logger.Info("starting automatic rollback")

	rollbackErr := upgrader.Rollback(ctx)
	if rollbackErr != nil {
		logger.Error(rollbackErr, "rollback failed")
		return command.ExitRollbackFailed
	}

	logger.Info("rollback completed successfully")
	return command.ExitRolledBack
}
