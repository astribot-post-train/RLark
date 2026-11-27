package rlarkadm

import (
	"context"
	"fmt"

	"github.com/rlinf/rlark/apps/rlark/pkg/log"
	"github.com/rlinf/rlark/apps/rlark/pkg/rlarkadm/types"
	"github.com/rlinf/rlark/apps/rlark/pkg/rlarkadm/upgrader"
	k8supgrade "github.com/rlinf/rlark/apps/rlark/pkg/rlarkadm/upgrader/kubernetes"
)

// Upgrade upgrades the components to match the provided deploy config.
// It generates the same resource manifests as install, but updates existing resources.
// Note: This function does not provide rollback capability. If upgrade fails,
// the caller should handle recovery (e.g., by re-running with the old config).
func Upgrade(cfg *types.DeployConfig) error {
	u, err := upgrader.New(cfg)
	if err != nil {
		return err
	}
	return runUpgrade(context.Background(), u, cfg, false)
}

// Rollback restores the supplied configuration even when current workloads
// are not healthy enough to pass the normal upgrade pre-check.
func Rollback(cfg *types.DeployConfig) error {
	u, err := upgrader.New(cfg)
	if err != nil {
		return err
	}
	return runUpgrade(context.Background(), u, cfg, true)
}

// UpgradeStockDataPlane applies the standard Kubernetes component profile.
// It is selected by explicitly opted-in stock batch children.
func UpgradeStockDataPlane(ctx context.Context, cfg *types.DeployConfig, rollback bool, check func(context.Context) error) error {
	u, err := k8supgrade.NewStockDataPlane(cfg, check)
	if err != nil {
		return err
	}
	return runUpgrade(ctx, u, cfg, rollback)
}

func runUpgrade(ctx context.Context, u upgrader.Upgrader, cfg *types.DeployConfig, rollback bool) error {
	logger := log.GetLogger()
	logger.Info("applying upgrade configuration", "plane", cfg.Plane, "mode", cfg.EnvMode(), "rollback", rollback)

	var err error
	if rollback {
		err = u.PreCheckForRollback(ctx, cfg)
	} else {
		err = u.PreCheck(ctx, cfg)
	}
	if err != nil {
		return fmt.Errorf("pre-check: %w", err)
	}
	if err := u.PreUpgrade(ctx, cfg); err != nil {
		return fmt.Errorf("pre-upgrade: %w", err)
	}
	if err := u.Upgrade(ctx, cfg); err != nil {
		return fmt.Errorf("upgrade: %w", err)
	}
	if err := u.PostUpgrade(ctx, cfg); err != nil {
		return fmt.Errorf("post-upgrade: %w", err)
	}
	logger.Info("plane upgrade completed", "plane", cfg.Plane, "rollback", rollback)
	if summary := u.Summary(); summary != nil {
		summary.Print()
	}
	return nil
}
