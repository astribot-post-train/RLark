package gatewayrelay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func authority(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test root"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	root, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(root)
	return root, key, pool
}
func leaf(t *testing.T, root *x509.Certificate, issuer *ecdsa.PrivateKey, name string, server bool) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	usage := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	if server {
		usage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usage}
	der, err := x509.CreateCertificate(rand.Reader, template, root, &key.PublicKey, issuer)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
func certPin(cert tls.Certificate) string {
	sum := sha256.Sum256(cert.Certificate[0])
	return hex.EncodeToString(sum[:])
}

type fixture struct {
	gateway   GatewayConfig
	hub       HubConfig
	agentTLS  *tls.Config
	address   string
	ctx       context.Context
	cancel    context.CancelFunc
	hubCancel context.CancelFunc
	hubDone   chan error
}

func await(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("component failed to stop")
	}
}
func newFixture(t *testing.T) *fixture {
	t.Helper()
	root, key, apiRoots := authority(t)
	apiCert := leaf(t, root, key, "api.test", true)
	nodeCert := leaf(t, root, key, "node-b", false)
	hr, hk, hRoots := authority(t)
	hubCert := leaf(t, hr, hk, "rlark-hub", false)
	gr, gk, gRoots := authority(t)
	gatewayCert := leaf(t, gr, gk, "gateway-a", true)
	apiListener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{apiCert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: apiRoots})
	require.NoError(t, err)
	var handlers sync.WaitGroup
	apiDone := make(chan struct{})
	go func() {
		defer close(apiDone)
		for {
			conn, err := apiListener.Accept()
			if err != nil {
				return
			}
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	hubs, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: hRoots, Certificates: []tls.Certificate{gatewayCert}})
	require.NoError(t, err)
	agents, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	f := &fixture{gateway: GatewayConfig{AgentListen: agents.Addr().String(), HubListen: hubs.Addr().String(), Certificate: gatewayCert, HubRoots: hRoots, HubPin: certPin(hubCert), MaxConnections: 8, IdleTimeout: time.Second}, hub: HubConfig{GatewayAddress: hubs.Addr().String(), GatewayServerName: "gateway-a", GatewayRoots: gRoots, GatewayPin: certPin(gatewayCert), Certificate: hubCert, APIAddress: apiListener.Addr().String(), MaxConnections: 8, IdleTimeout: time.Second}, agentTLS: &tls.Config{MinVersion: tls.VersionTLS13, ServerName: "api.test", RootCAs: apiRoots, Certificates: []tls.Certificate{nodeCert}}, address: agents.Addr().String(), ctx: ctx, cancel: cancel}
	done := make(chan error, 1)
	go func() { done <- serveGateway(ctx, f.gateway, hubs, agents) }()
	t.Cleanup(func() {
		if f.hubCancel != nil {
			f.hubCancel()
			await(t, f.hubDone)
		}
		cancel()
		await(t, done)
		_ = apiListener.Close()
		<-apiDone
		handlers.Wait()
	})
	return f
}
func (f *fixture) startHub() {
	ctx, cancel := context.WithCancel(f.ctx)
	f.hubCancel = cancel
	f.hubDone = make(chan error, 1)
	go func() { f.hubDone <- RunHub(ctx, f.hub) }()
}
func (f *fixture) ping() bool {
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 300 * time.Millisecond}, Config: f.agentTLS}
	ctx, cancel := context.WithTimeout(f.ctx, 300*time.Millisecond)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", f.address)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err = conn.Write([]byte("agent-b")); err != nil {
		return false
	}
	got := make([]byte, 7)
	_, err = io.ReadFull(conn, got)
	return err == nil && string(got) == "agent-b"
}
func TestRelayPreservesNodeMTLSAndRecoversHubReplacement(t *testing.T) {
	f := newFixture(t)
	f.startHub()
	require.Eventually(t, f.ping, 5*time.Second, 20*time.Millisecond)
	// The gateway certificate and Hub credential cannot stand in for a node
	// certificate at the end-to-end API. Nor can an untrusted API be accepted.
	good := f.agentTLS
	f.agentTLS = good.Clone()
	f.agentTLS.Certificates = []tls.Certificate{f.hub.Certificate}
	require.False(t, f.ping())
	f.agentTLS = good.Clone()
	f.agentTLS.RootCAs = x509.NewCertPool()
	require.False(t, f.ping())
	f.agentTLS = good
	f.hubCancel()
	await(t, f.hubDone)
	f.hubCancel = nil
	require.False(t, f.ping())
	f.startHub()
	require.Eventually(t, f.ping, 5*time.Second, 20*time.Millisecond)
}
func TestRelayRejectsWrongPinsAndRecoversConfiguration(t *testing.T) {
	for _, side := range []string{"gateway", "hub"} {
		t.Run(side, func(t *testing.T) {
			f := newFixture(t)
			if side == "hub" {
				f.hub.GatewayPin = strings.Repeat("0", 64)
			} else {
				f.hub.Certificate = f.agentTLS.Certificates[0]
			}
			f.startHub()
			require.Never(t, f.ping, 500*time.Millisecond, 20*time.Millisecond)
			f.hubCancel()
			await(t, f.hubDone)
			f.hubCancel = nil
		})
	}
}
func TestRelayIdleTimeoutAndBusyShutdown(t *testing.T) {
	f := newFixture(t)
	f.startHub()
	require.Eventually(t, f.ping, 5*time.Second, 20*time.Millisecond)
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", f.address, f.agentTLS)
	require.NoError(t, err)
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var buf [1]byte
	_, err = conn.Read(buf[:])
	require.Error(t, err)
	f.hubCancel()
	await(t, f.hubDone)
	f.hubCancel = nil
	f.startHub()
	require.Eventually(t, f.ping, 5*time.Second, 20*time.Millisecond)
	conn2, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", f.address, f.agentTLS)
	require.NoError(t, err)
	defer conn2.Close()
	f.hubCancel()
	await(t, f.hubDone)
	f.hubCancel = nil
	_ = conn2.SetReadDeadline(time.Now().Add(time.Second))
	_, err = conn2.Read(buf[:])
	require.Error(t, err)
}
func TestGatewayValidation(t *testing.T) {
	f := newFixture(t)
	for _, addr := range []string{"0.0.0.0:30000", "8.8.8.8:30000", "[::]:30000", "localhost:30000", "10.1.2.3"} {
		c := f.gateway
		c.AgentListen = addr
		require.Error(t, c.Validate())
	}
	c := f.gateway
	c.MaxConnections = 0
	require.Error(t, c.Validate())
	c.MaxConnections = 4097
	require.Error(t, c.Validate())
	c = f.gateway
	c.HubPin = ""
	require.Error(t, c.Validate())
	c = f.gateway
	c.IdleTimeout = 0
	require.Error(t, c.Validate())
	require.NoError(t, f.gateway.Validate())
	require.NoError(t, f.hub.Validate())
}
