// Command container-runtime-gateway provides the optional A/Hub restricted
// relay. It is deliberately separate from the Agent and never proxies an
// arbitrary address: Hub mode dials one configured extension API only.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/gatewayrelay"
	"github.com/rlinf/rlark/apps/rlark/pkg/log"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.FromContext(ctx).Error(err, "container gateway failed")
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("container-runtime-gateway", flag.ContinueOnError)
	var mode, agentListen, hubListen, gatewayAddress, apiAddress, serverCert, serverKey, clientCert, clientKey, rootsFile, pin, serverName string
	var max int
	var idle time.Duration
	fs.StringVar(&mode, "mode", "", "gateway or hub")
	fs.StringVar(&agentListen, "agent-listen", "", "private Agent TCP address (gateway mode)")
	fs.StringVar(&hubListen, "hub-listen", "", "mTLS Hub listen address (gateway mode)")
	fs.StringVar(&gatewayAddress, "gateway-address", "", "A mTLS address (hub mode)")
	fs.StringVar(&apiAddress, "api-address", "", "fixed local extension API address (hub mode)")
	fs.StringVar(&serverCert, "server-cert", "", "gateway server certificate")
	fs.StringVar(&serverKey, "server-key", "", "gateway server private key")
	fs.StringVar(&clientCert, "client-cert", "", "Hub client certificate")
	fs.StringVar(&clientKey, "client-key", "", "Hub client private key")
	fs.StringVar(&rootsFile, "roots", "", "CA bundle for the opposite side")
	fs.StringVar(&pin, "peer-cert-sha256", "", "lowercase SHA-256 pin for peer leaf certificate")
	fs.StringVar(&serverName, "server-name", "", "gateway TLS server name (hub mode)")
	fs.IntVar(&max, "max-connections", 64, "maximum concurrent relay connections")
	fs.DurationVar(&idle, "idle-timeout", 5*time.Minute, "per-direction idle timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("positional arguments are unsupported")
	}
	if mode != "gateway" && mode != "hub" {
		return errors.New("mode must be gateway or hub")
	}
	if rootsFile == "" || pin == "" {
		return errors.New("roots and peer-cert-sha256 are required")
	}
	roots, err := loadRoots(rootsFile)
	if err != nil {
		return err
	}
	if mode == "gateway" {
		cert, err := tls.LoadX509KeyPair(serverCert, serverKey)
		if err != nil {
			return fmt.Errorf("load gateway certificate: %w", err)
		}
		return gatewayrelay.RunGateway(ctx, gatewayrelay.GatewayConfig{AgentListen: agentListen, HubListen: hubListen, Certificate: cert, HubRoots: roots, HubPin: pin, MaxConnections: max, IdleTimeout: idle})
	}
	client, err := tls.LoadX509KeyPair(clientCert, clientKey)
	if err != nil {
		return fmt.Errorf("load Hub certificate: %w", err)
	}
	return gatewayrelay.RunHub(ctx, gatewayrelay.HubConfig{GatewayAddress: gatewayAddress, GatewayServerName: serverName, GatewayRoots: roots, GatewayPin: pin, Certificate: client, APIAddress: apiAddress, MaxConnections: max, IdleTimeout: idle})
}

func loadRoots(path string) (*x509.CertPool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CA bundle: %w", err)
	}
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(b) {
		return nil, errors.New("CA bundle contains no certificate")
	}
	return p, nil
}
