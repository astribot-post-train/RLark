//go:build linux

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/agent"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw/supervision"
)

type Config struct {
	Endpoint, TLSCert, TLSKey, ServerCA, Version, PrivateAddress, NvidiaSMI, CgroupMount, SupervisorSocket string
	Interval, RetryMax                                                                                     time.Duration
	EnableExecution                                                                                        bool
}

func DefaultConfig() Config {
	return Config{NvidiaSMI: "nvidia-smi", CgroupMount: "/sys/fs/cgroup", Interval: 15 * time.Second, RetryMax: 2 * time.Minute}
}
func (c Config) Validate() error {
	if c.Endpoint == "" || c.TLSCert == "" || c.TLSKey == "" || c.ServerCA == "" || c.Version == "" || c.PrivateAddress == "" || c.SupervisorSocket == "" {
		return errors.New("endpoint, TLS files, version, private address and supervisor socket are required")
	}
	if c.Interval <= 0 || c.RetryMax <= 0 || c.RetryMax < c.Interval || c.RetryMax > 5*time.Minute || c.Interval > 15*time.Second {
		return errors.New("positive retry interval and retry max are required")
	}
	return nil
}
func (c *Config) SetupFlags(fs *flag.FlagSet) {
	fs.BoolVar(&c.EnableExecution, "enable-task-execution", c.EnableExecution, "enable preview scoped Raw process execution")
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
	observer := agent.LocalObserver{Version: cfg.Version, PrivateAddress: cfg.PrivateAddress, NvidiaSMI: cfg.NvidiaSMI, CgroupMount: cfg.CgroupMount, SupervisorSocket: cfg.SupervisorSocket}
	defer client.HTTPClient.CloseIdleConnections()
	if cfg.EnableExecution {
		supervisor, err := supervision.NewClient(cfg.SupervisorSocket)
		if err != nil {
			return err
		}
		defer supervisor.CloseIdleConnections()
		execution := agent.ExecutionObserver{Observer: observer, Client: client, Supervisor: supervisor}
		return agent.RunExecution(ctx, execution, cfg.Interval, cfg.RetryMax, logger)
	}
	return agent.Run(ctx, observer, client, cfg.Interval, cfg.RetryMax, logger)
}
