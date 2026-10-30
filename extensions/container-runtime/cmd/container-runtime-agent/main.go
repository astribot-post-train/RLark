//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/agent"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw/supervision"
)

type Config struct {
	Endpoint, TLSCert, TLSKey, ServerCA, Version, PrivateAddress, NvidiaSMI, CgroupMount, SupervisorSocket string
	Interval, RetryMax                                                                                     time.Duration
	EnableExecution, EnableUpgrades, EnableLogReads                                                        bool
	InstallPath, UpgradeStateDirectory                                                                     string
}

func DefaultConfig() Config {
	return Config{NvidiaSMI: "nvidia-smi", CgroupMount: "/sys/fs/cgroup", Interval: 15 * time.Second, RetryMax: 2 * time.Minute}
}
func (c Config) Validate() error {
	if c.EnableLogReads && !c.EnableExecution {
		return errors.New("log reads require scoped task execution")
	}
	if c.Endpoint == "" || c.TLSCert == "" || c.TLSKey == "" || c.ServerCA == "" || c.Version == "" || c.PrivateAddress == "" || c.SupervisorSocket == "" {
		return errors.New("endpoint, TLS files, version, private address and supervisor socket are required")
	}
	if c.Interval <= 0 || c.RetryMax <= 0 || c.RetryMax < c.Interval || c.RetryMax > 5*time.Minute || c.Interval > 15*time.Second {
		return errors.New("positive retry interval and retry max are required")
	}
	if c.EnableUpgrades {
		if err := (agent.UpdaterPaths{InstallPath: c.InstallPath, StateDirectory: c.UpgradeStateDirectory}).Validate(); err != nil {
			return err
		}
		if filepath.Dir(c.SupervisorSocket) == c.UpgradeStateDirectory || strings.HasPrefix(c.UpgradeStateDirectory, filepath.Dir(c.SupervisorSocket)+"/") || strings.HasPrefix(filepath.Dir(c.SupervisorSocket), c.UpgradeStateDirectory+"/") {
			return errors.New("supervisor and updater state directories must be separate")
		}
	}
	return nil
}
func (c *Config) SetupFlags(fs *flag.FlagSet) {
	fs.BoolVar(&c.EnableUpgrades, "enable-agent-upgrades", c.EnableUpgrades, "enable preview idle Agent binary updates")
	fs.StringVar(&c.InstallPath, "agent-install-path", c.InstallPath, "absolute installed Agent executable path")
	fs.StringVar(&c.UpgradeStateDirectory, "upgrade-state-directory", c.UpgradeStateDirectory, "private existing upgrade journal directory")
	fs.BoolVar(&c.EnableExecution, "enable-task-execution", c.EnableExecution, "enable preview scoped Raw process execution")
	fs.BoolVar(&c.EnableLogReads, "enable-log-reads", c.EnableLogReads, "enable on-demand scoped Raw log snapshots")
	fs.StringVar(&c.Endpoint, "endpoint", c.Endpoint, "extension API HTTPS origin")
	fs.StringVar(&c.TLSCert, "tls-cert", c.TLSCert, "node Agent client certificate")
	fs.StringVar(&c.TLSKey, "tls-key", c.TLSKey, "node Agent client private key")
	fs.StringVar(&c.ServerCA, "server-ca", c.ServerCA, "extension API server CA")
	fs.StringVar(&c.Version, "version", c.Version, "Agent release version")
	fs.StringVar(&c.PrivateAddress, "private-address", c.PrivateAddress, "node private IPv4 address")
	fs.StringVar(&c.NvidiaSMI, "nvidia-smi", c.NvidiaSMI, "GPU visibility executable")
	fs.StringVar(&c.CgroupMount, "cgroup-mount", c.CgroupMount, "cgroup v2 mount")
	fs.StringVar(&c.SupervisorSocket, "supervisor-socket", c.SupervisorSocket, "local Raw supervisor socket")
	fs.DurationVar(&c.Interval, "interval", c.Interval, "successful heartbeat interval")
	fs.DurationVar(&c.RetryMax, "retry-max", c.RetryMax, "maximum outage retry delay")
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	cfg := DefaultConfig()
	cfg.SetupFlags(flag.CommandLine)
	flag.Parse()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(ctx, cfg, logger); err != nil {
		logger.Error("container-runtime Agent stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg Config, logger *slog.Logger) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	certificate, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
	if err != nil {
		return fmt.Errorf("load Agent certificate: %w", err)
	}
	data, err := os.ReadFile(cfg.ServerCA)
	if err != nil {
		return fmt.Errorf("read server CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data) {
		return errors.New("server CA contains no certificate")
	}
	client, err := agent.NewClient(cfg.Endpoint, certificate, roots)
	if err != nil {
		return err
	}
	var updater *agent.Updater
	if cfg.EnableUpgrades {
		hash := sha256.New()
		_, _ = hash.Write([]byte(cfg.Endpoint + "\x00"))
		_, _ = hash.Write(certificate.Certificate[0]) // Hash.Write cannot fail.
		updater, err = agent.NewUpdater(agent.UpdaterConfig{Paths: agent.UpdaterPaths{InstallPath: cfg.InstallPath, StateDirectory: cfg.UpgradeStateDirectory}, Scope: hex.EncodeToString(hash.Sum(nil)), BootstrapVersion: cfg.Version}, client, func(ctx context.Context, path string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return syscall.Exec(path, append([]string{path}, os.Args[1:]...), os.Environ())
		})
		if err != nil {
			return err
		}
		defer updater.Close()
		cfg.Version = updater.Version()
	}
	observer := agent.LocalObserver{Version: cfg.Version, PrivateAddress: cfg.PrivateAddress, NvidiaSMI: cfg.NvidiaSMI, CgroupMount: cfg.CgroupMount, SupervisorSocket: cfg.SupervisorSocket, EnableUpgrades: cfg.EnableUpgrades}
	defer client.HTTPClient.CloseIdleConnections()
	if cfg.EnableExecution {
		supervisor, err := supervision.NewClient(cfg.SupervisorSocket)
		if err != nil {
			return err
		}
		defer supervisor.CloseIdleConnections()
		if cfg.EnableLogReads {
			logCtx, cancelLogs := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() { defer close(done); agent.RunLogReads(logCtx, client, supervisor) }()
			defer func() { cancelLogs(); <-done }()
		}
		execution := agent.ExecutionObserver{Observer: observer, Client: client, Supervisor: supervisor, Updater: updater}
		return agent.RunExecution(ctx, execution, cfg.Interval, cfg.RetryMax, logger)
	}
	var heartbeater agent.Heartbeater = client
	if updater != nil {
		heartbeater = agent.UpgradingHeartbeater{Client: client, Updater: updater}
	}
	return agent.Run(ctx, observer, heartbeater, cfg.Interval, cfg.RetryMax, logger)
}
