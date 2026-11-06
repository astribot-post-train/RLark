// Package gatewayrelay carries Agent API connections through a gateway. The
// Agent's TLS connection stays end-to-end and the destination is fixed.
package gatewayrelay

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rlinf/rlark/apps/rlark/pkg/log"
	"github.com/xtaci/smux"
)

// GatewayConfig configures A. Pins are lowercase SHA-256 of leaf certificate DER.
type GatewayConfig struct {
	AgentListen, HubListen string
	Certificate            tls.Certificate
	HubRoots               *x509.CertPool
	HubPin                 string
	MaxConnections         int
	IdleTimeout            time.Duration
}

// HubConfig configures RLark's outgoing link and single fixed destination.
type HubConfig struct {
	GatewayAddress, GatewayServerName string
	GatewayRoots                      *x509.CertPool
	GatewayPin                        string
	Certificate                       tls.Certificate
	APIAddress                        string
	MaxConnections                    int
	IdleTimeout                       time.Duration
}

func validPin(pin string) bool {
	if len(pin) != sha256.Size*2 || strings.ToLower(pin) != pin {
		return false
	}
	_, err := hex.DecodeString(pin)
	return err == nil
}
func pinMatches(cert *x509.Certificate, want string) bool {
	if cert == nil || !validPin(want) {
		return false
	}
	got := sha256.Sum256(cert.Raw)
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(got[:])), []byte(want)) == 1
}
func validCertificate(c tls.Certificate) bool { return len(c.Certificate) != 0 && c.PrivateKey != nil }
func validLimits(max int, idle time.Duration) bool {
	return max >= 1 && max <= 256 && idle >= time.Second && idle <= 30*time.Minute
}
func validAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	return err == nil && host != "" && validPort(port)
}

func validPort(port string) bool {
	value, err := strconv.Atoi(port)
	return err == nil && value >= 1 && value <= 65535
}

// Validate rejects exposed Agent listeners, missing identity pins and invalid limits.
func (c GatewayConfig) Validate() error {
	host, port, err := net.SplitHostPort(c.AgentListen)
	ip := net.ParseIP(host)
	if err != nil || !validPort(port) || ip == nil || !(ip.IsPrivate() || ip.IsLoopback()) {
		return errors.New("Agent listener must use an explicit private or loopback IP")
	}
	if _, port, err := net.SplitHostPort(c.HubListen); err != nil || !validPort(port) {
		return errors.New("invalid Hub listener")
	}
	if !validCertificate(c.Certificate) || c.HubRoots == nil || !validPin(c.HubPin) {
		return errors.New("gateway certificate, Hub CA and Hub leaf pin required")
	}
	if !validLimits(c.MaxConnections, c.IdleTimeout) {
		return errors.New("invalid connection limit or idle timeout")
	}
	return nil
}

// Validate requires authenticated TLS and an operator-configured fixed API address.
func (c HubConfig) Validate() error {
	if !validAddress(c.GatewayAddress) || !validAddress(c.APIAddress) || c.GatewayServerName == "" {
		return errors.New("gateway address, TLS server name and fixed API address required")
	}
	if !validCertificate(c.Certificate) || c.GatewayRoots == nil || !validPin(c.GatewayPin) {
		return errors.New("Hub certificate, gateway CA and gateway leaf pin required")
	}
	if !validLimits(c.MaxConnections, c.IdleTimeout) {
		return errors.New("invalid connection limit or idle timeout")
	}
	return nil
}

func muxConfig() *smux.Config {
	c := smux.DefaultConfig()
	c.Version = 2
	c.MaxReceiveBuffer = 4 << 20
	c.MaxStreamBuffer = 64 << 10
	c.KeepAliveInterval = 10 * time.Second
	c.KeepAliveTimeout = 30 * time.Second
	return c
}

type gatewayState struct {
	mu      sync.Mutex
	session *smux.Session
}

func (s *gatewayState) set(next *smux.Session) {
	s.mu.Lock()
	old := s.session
	s.session = next
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}
func (s *gatewayState) clear(current *smux.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == current {
		s.session = nil
	}
}
func (s *gatewayState) get() *smux.Session { s.mu.Lock(); defer s.mu.Unlock(); return s.session }

// RunGateway accepts a pinned mTLS Hub and relays private Agent connections.
// A reconnect replaces the previous session and drops its in-flight streams.
func RunGateway(ctx context.Context, c GatewayConfig) error {
	if err := c.Validate(); err != nil {
		return err
	}
	hubs, err := tls.Listen("tcp", c.HubListen, &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: c.HubRoots.Clone(), Certificates: []tls.Certificate{c.Certificate}})
	if err != nil {
		return fmt.Errorf("listen for Hub: %w", err)
	}
	agents, err := net.Listen("tcp", c.AgentListen)
	if err != nil {
		_ = hubs.Close()
		return fmt.Errorf("listen for Agents: %w", err)
	}
	return serveGateway(ctx, c, hubs, agents)
}

func serveGateway(ctx context.Context, c GatewayConfig, hubs, agents net.Listener) error {
	defer hubs.Close()
	defer agents.Close()
	var state gatewayState
	var handlers, loops sync.WaitGroup
	var errMu sync.Mutex
	var serveErr error
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	fail := func(err error) {
		if ctx.Err() == nil {
			errMu.Lock()
			if serveErr == nil {
				serveErr = err
			}
			errMu.Unlock()
			cancel()
		}
	}
	shutdown := make(chan struct{})
	go func() { defer close(shutdown); <-ctx.Done(); _ = hubs.Close(); _ = agents.Close(); state.set(nil) }()
	handshakes := make(chan struct{}, 4)
	connections := make(chan struct{}, c.MaxConnections)
	loops.Add(2)
	go func() {
		defer loops.Done()
		for {
			conn, err := hubs.Accept()
			if err != nil {
				fail(fmt.Errorf("accept Hub connection: %w", err))
				return
			}
			select {
			case handshakes <- struct{}{}:
			default:
				_ = conn.Close()
				continue
			}
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				defer func() { <-handshakes }()
				tlsConn, ok := conn.(*tls.Conn)
				if !ok {
					_ = conn.Close()
					return
				}
				hctx, stop := context.WithTimeout(ctx, 5*time.Second)
				err := tlsConn.HandshakeContext(hctx)
				stop()
				if err != nil || len(tlsConn.ConnectionState().PeerCertificates) == 0 || !pinMatches(tlsConn.ConnectionState().PeerCertificates[0], c.HubPin) {
					_ = conn.Close()
					return
				}
				if ctx.Err() != nil {
					_ = conn.Close()
					return
				}
				session, err := smux.Server(tlsConn, muxConfig())
				if err != nil {
					_ = conn.Close()
					return
				}
				state.set(session)
				log.FromContext(ctx).Info("container gateway Hub session connected")
				select {
				case <-session.CloseChan():
				case <-ctx.Done():
					_ = session.Close()
				}
				state.clear(session)
				log.FromContext(ctx).Info("container gateway Hub session disconnected")
			}()
		}
	}()
	go func() {
		defer loops.Done()
		for {
			conn, err := agents.Accept()
			if err != nil {
				fail(fmt.Errorf("accept Agent connection: %w", err))
				return
			}
			select {
			case connections <- struct{}{}:
			default:
				_ = conn.Close()
				continue
			}
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				defer func() { <-connections; _ = conn.Close() }()
				session := state.get()
				if session == nil || session.IsClosed() {
					return
				}
				stream, err := session.OpenStream()
				if err != nil {
					return
				}
				relay(conn, stream, c.IdleTimeout)
			}()
		}
	}()
	loops.Wait()
	cancel()
	<-shutdown
	handlers.Wait()
	return serveErr
}

// RunHub reconnects to A and forwards streams only to the fixed extension API.
// The destination's mTLS API validates the independent node credentials.
func RunHub(ctx context.Context, c HubConfig) error {
	if err := c.Validate(); err != nil {
		return err
	}
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}, Config: &tls.Config{
			MinVersion: tls.VersionTLS13, ServerName: c.GatewayServerName, RootCAs: c.GatewayRoots.Clone(), Certificates: []tls.Certificate{c.Certificate},
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 || !pinMatches(cs.PeerCertificates[0], c.GatewayPin) {
					return errors.New("gateway certificate pin mismatch")
				}
				return nil
			},
		}}
		dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		conn, err := dialer.DialContext(dialCtx, "tcp", c.GatewayAddress)
		cancel()
		if err == nil {
			log.FromContext(ctx).Info("container Hub connected to gateway")
			serveHubSession(ctx, c, conn)
			log.FromContext(ctx).Info("container Hub gateway session disconnected")
		} else if ctx.Err() == nil {
			// Arbitrary TLS/dial errors may contain sensitive infrastructure details.
			log.FromContext(ctx).V(1).Info("container Hub gateway connection unavailable; retrying")
		}
		if ctx.Err() != nil {
			break
		}
		if time.Since(started) > 30*time.Second {
			backoff = time.Second
		}
		if !sleep(ctx, backoff) {
			break
		}
		if backoff < 16*time.Second {
			backoff *= 2
		}
	}
	return nil
}

func serveHubSession(ctx context.Context, c HubConfig, conn net.Conn) {
	session, err := smux.Client(conn, muxConfig())
	if err != nil {
		_ = conn.Close()
		return
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = session.Close()
		case <-done:
		}
	}()
	var handlers sync.WaitGroup
	defer func() { _ = session.Close(); close(done); handlers.Wait() }()
	connections := make(chan struct{}, c.MaxConnections)
	for {
		stream, err := session.AcceptStream()
		if err != nil {
			return
		}
		select {
		case connections <- struct{}{}:
		default:
			_ = stream.Close()
			continue
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			defer func() { <-connections; _ = stream.Close() }()
			dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			upstream, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", c.APIAddress)
			cancel()
			if err != nil {
				return
			}
			relay(stream, upstream, c.IdleTimeout)
		}()
	}
}

func sleep(ctx context.Context, duration time.Duration) bool {
	t := time.NewTimer(duration)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

type idleConn struct {
	net.Conn
	timeout time.Duration
}

func (c idleConn) Read(b []byte) (int, error) {
	if err := c.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Read(b)
}
func (c idleConn) Write(b []byte) (int, error) {
	if err := c.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}
func relay(left, right net.Conn, idle time.Duration) {
	defer left.Close()
	defer right.Close()
	type copyResult struct{ err error }
	results := make(chan copyResult, 2)
	copyData := func(dst, src net.Conn) {
		_, err := io.Copy(idleConn{dst, idle}, idleConn{src, idle})
		if err == nil {
			if half, ok := dst.(interface{ CloseWrite() error }); ok {
				err = half.CloseWrite()
			}
		}
		results <- copyResult{err: err}
	}
	go copyData(left, right)
	go copyData(right, left)
	first := <-results
	if first.err != nil {
		_ = left.Close()
		_ = right.Close()
	}
	<-results
	_ = left.Close()
	_ = right.Close()
}
