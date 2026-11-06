package rayrelay

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/sync/errgroup"
)

// ServiceConfig is a local, operator-owned preview transport configuration.
// It contains paths to dedicated transport credentials, never workload credentials.
// A peer roster is an explicit allowlist; it is not inferred from reachable hosts.
type ServiceConfig struct {
	Mode           string                `json:"mode"`
	Listen         string                `json:"listen"`
	Certificate    string                `json:"certificate"`
	PrivateKey     string                `json:"privateKey"`
	CA             string                `json:"ca"`
	GatewayAddress string                `json:"gatewayAddress,omitempty"`
	LocalAddress   string                `json:"localAddress,omitempty"`
	ServerName     string                `json:"serverName,omitempty"`
	GatewayPin     string                `json:"gatewayPin,omitempty"`
	NodeID         string                `json:"nodeID,omitempty"`
	Peers          map[string]PeerAccess `json:"peers"`
	MaxNodes       int                   `json:"maxNodes,omitempty"`
	MaxStreams     int                   `json:"maxStreams"`
	IdleSeconds    int                   `json:"idleSeconds"`
}

// LoadServiceConfig rejects partial, unknown and ambiguous local configuration.
// Configuration and credential rotation take effect on a targeted process restart.
func LoadServiceConfig(path, mode string) (ServiceConfig, error) {
	var cfg ServiceConfig
	f, err := os.Open(path)
	if err != nil {
		return cfg, fmt.Errorf("open Ray transport configuration: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return cfg, fmt.Errorf("read Ray transport configuration: %w", err)
	}
	if len(data) > 1<<20 {
		return cfg, errors.New("Ray transport configuration exceeds the preview limit")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		// Do not echo configuration contents or parser errors that may include them.
		return cfg, errors.New("invalid Ray transport configuration")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return cfg, errors.New("exactly one Ray transport configuration is required")
	}
	if cfg.Mode != mode || (mode != "gateway" && mode != "peer") || cfg.Listen == "" || cfg.Certificate == "" || cfg.PrivateKey == "" || cfg.CA == "" || cfg.IdleSeconds < 1 || cfg.IdleSeconds > 1800 {
		return cfg, errors.New("Ray transport mode, listener, credentials and bounded idleSeconds are required")
	}
	if mode == "gateway" && (cfg.GatewayAddress != "" || cfg.LocalAddress != "" || cfg.ServerName != "" || cfg.GatewayPin != "" || cfg.NodeID != "") {
		return cfg, errors.New("Ray gateway does not accept peer-only fields")
	}
	if mode == "peer" && cfg.MaxNodes != 0 {
		return cfg, errors.New("Ray peer does not accept gateway maxNodes")
	}
	return cfg, nil
}

func (c ServiceConfig) credentials() (tls.Certificate, *x509.CertPool, error) {
	info, err := os.Lstat(c.PrivateKey)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return tls.Certificate{}, nil, errors.New("Ray transport private key must be a private regular file")
	}
	cert, err := tls.LoadX509KeyPair(c.Certificate, c.PrivateKey)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("load Ray transport certificate: %w", err)
	}
	data, err := os.ReadFile(c.CA)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("read Ray transport CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data) {
		return tls.Certificate{}, nil, errors.New("Ray transport CA contains no certificate")
	}
	return cert, roots, nil
}

// LoadGateway prepares the optional A data service without starting goroutines.
func LoadGateway(path string) (*Server, error) {
	cfg, err := LoadServiceConfig(path, "gateway")
	if err != nil {
		return nil, err
	}
	cert, roots, err := cfg.credentials()
	if err != nil {
		return nil, err
	}
	return NewServer(Config{Listen: cfg.Listen, Certificate: cert, ClientRoots: roots, Peers: cfg.Peers, MaxNodes: cfg.MaxNodes, MaxStreams: cfg.MaxStreams, IdleTimeout: time.Duration(cfg.IdleSeconds) * time.Second})
}

// PeerService maintains the authenticated peer session and loopback connector.
// It never sets LD_PRELOAD, changes routes or starts a Ray process.
type PeerService struct {
	Client *Client
	Listen string
}

// LoadPeer prepares a dedicated peer service without starting goroutines.
func LoadPeer(path string) (*PeerService, error) {
	cfg, err := LoadServiceConfig(path, "peer")
	if err != nil {
		return nil, err
	}
	cert, roots, err := cfg.credentials()
	if err != nil {
		return nil, err
	}
	c := &Client{Address: cfg.GatewayAddress, LocalAddress: cfg.LocalAddress, ServerName: cfg.ServerName, Roots: roots, Certificate: cert, Pin: cfg.GatewayPin, NodeID: cfg.NodeID, Peers: cfg.Peers, MaxStreams: cfg.MaxStreams, IdleTimeout: time.Duration(cfg.IdleSeconds) * time.Second}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if cert.Leaf == nil || cert.Leaf.Subject.CommonName != cfg.NodeID {
		return nil, errors.New("Ray peer certificate does not match nodeID")
	}
	if err := validateConnectorAddress(cfg.Listen); err != nil {
		return nil, err
	}
	return &PeerService{Client: c, Listen: cfg.Listen}, nil
}

// Run owns both loops: connector failure cancels the session, and cancellation
// closes active streams. Reconnection admits new streams, never replays bytes.
func (p *PeerService) Run(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return p.Client.Run(ctx) })
	g.Go(func() error { return p.Client.ServeConnector(ctx, p.Listen) })
	return g.Wait()
}
