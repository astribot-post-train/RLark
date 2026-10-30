//go:build linux

package main

import (
	"flag"
	"testing"
	"time"
)

func configured() Config {
	cfg := DefaultConfig()
	cfg.Endpoint = "https://api.example.test"
	cfg.TLSCert = "/private/cert.pem"
	cfg.TLSKey = "/private/key.pem"
	cfg.ServerCA = "/private/ca.pem"
	cfg.Version = "v1"
	cfg.PrivateAddress = "10.0.3.85"
	cfg.SupervisorSocket = "/private/supervisor.sock"
	return cfg
}
func TestAgentConfigAndFlags(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.Validate(); err == nil {
		t.Fatal("accepted missing identity")
	}
	cfg = configured()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg.SetupFlags(fs)
	if err := fs.Parse([]string{"--interval=10s", "--retry-max=1m"}); err != nil {
		t.Fatal(err)
	}
	if cfg.Interval != 10*time.Second || cfg.RetryMax != time.Minute {
		t.Fatal(cfg)
	}
	for _, invalid := range []func(*Config){func(c *Config) { c.Interval = 0 }, func(c *Config) { c.Interval = 16 * time.Second }, func(c *Config) { c.RetryMax = 6 * time.Minute }, func(c *Config) { c.SupervisorSocket = "" }} {
		bad := configured()
		invalid(&bad)
		if err := bad.Validate(); err == nil {
			t.Fatalf("invalid config accepted: %+v", bad)
		}
	}
}

func TestUpgradeFlagsRequireIsolatedPaths(t *testing.T) {
	cfg := configured()
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	cfg.SetupFlags(fs)
	if err := fs.Parse([]string{"--enable-agent-upgrades=true", "--agent-install-path=/opt/agent/container-runtime-agent", "--upgrade-state-directory=/var/lib/agent-upgrades"}); err != nil {
		t.Fatal(err)
	}
	if !cfg.EnableUpgrades {
		t.Fatal("upgrade flag missing")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", "relative", "/opt/agent/state", "/private", "/private/child"} {
		bad := cfg
		bad.UpgradeStateDirectory = path
		if err := bad.Validate(); err == nil {
			t.Fatalf("accepted shared/nonabsolute path %q", path)
		}
	}
}

func TestLogReadsRequireScopedExecution(t *testing.T) {
	cfg := configured()
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	cfg.SetupFlags(fs)
	if err := fs.Parse([]string{"--enable-log-reads=true"}); err != nil {
		t.Fatal(err)
	}
	if !cfg.EnableLogReads || cfg.Validate() == nil {
		t.Fatal("log reads accepted without scoped execution")
	}
	cfg.EnableExecution = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDockerModeHasNoSupervisorOrExecutionCapabilities(t *testing.T) {
	cfg := configured()
	cfg.SupervisorSocket = ""
	fs := flag.NewFlagSet("docker", flag.ContinueOnError)
	cfg.SetupFlags(fs)
	if err := fs.Parse([]string{"--runtime=docker", "--docker-host=unix:///run/docker.sock"}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.SupervisorSocket = "/raw/supervisor.sock" },
		func(c *Config) { c.EnableExecution = true },
		func(c *Config) { c.EnableLogReads = true },
		func(c *Config) { c.EnableUpgrades = true },
		func(c *Config) { c.Runtime = "unknown" },
		func(c *Config) { c.DockerHost = "tcp://127.0.0.1:2375" },
		func(c *Config) { c.DockerHost = "unix:///run/../docker.sock" },
	} {
		bad := cfg
		mutate(&bad)
		if err := bad.Validate(); err == nil {
			t.Fatalf("accepted unsupported Docker config: %+v", bad)
		}
	}
	bad := configured()
	bad.DockerHost = "unix:///run/docker.sock"
	if err := bad.Validate(); err == nil {
		t.Fatal("Raw mode accepted Docker host")
	}
}

func TestDockerExecutionRequiresSeparateCanonicalState(t *testing.T) {
	cfg := configured()
	cfg.Runtime = "docker"
	cfg.SupervisorSocket = ""
	cfg.EnableExecution = true
	for _, path := range []string{"", "relative", "/", "/var/../state"} {
		cfg.StateDirectory = path
		if cfg.Validate() == nil {
			t.Fatalf("unsafe state accepted %q", path)
		}
	}
	cfg.StateDirectory = "/var/lib/rlark-docker"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Runtime = "raw"
	cfg.SupervisorSocket = "/private/supervisor.sock"
	if cfg.Validate() == nil {
		t.Fatal("Raw accepted Docker state")
	}
}

func TestDockerDiagnosticsRequireExplicitDockerExecution(t *testing.T) {
	cfg := configured()
	cfg.Runtime = "docker"
	cfg.SupervisorSocket = ""
	fs := flag.NewFlagSet("diagnostics", flag.ContinueOnError)
	cfg.SetupFlags(fs)
	if err := fs.Parse([]string{"--enable-docker-diagnostics=true"}); err != nil {
		t.Fatal(err)
	}
	if cfg.Validate() == nil {
		t.Fatal("accepted diagnostics without execution")
	}
	cfg.EnableExecution = true
	cfg.StateDirectory = "/var/lib/rlark-docker"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Runtime = "raw"
	cfg.StateDirectory = ""
	cfg.SupervisorSocket = "/private/supervisor.sock"
	if cfg.Validate() == nil {
		t.Fatal("Raw accepted Docker diagnostics")
	}
}
