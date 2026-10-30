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
