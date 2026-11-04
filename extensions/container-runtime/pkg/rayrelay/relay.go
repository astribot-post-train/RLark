// Package rayrelay provides an authenticated, single-listener TCP stream
// broker for Ray peers that cannot route directly to one another.
package rayrelay

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xtaci/smux"
)

const maxRequestBytes = 4096

type Config struct {
	Listen      string
	Certificate tls.Certificate
	ClientRoots *x509.CertPool
	Peers       map[string]PeerAccess
	MaxNodes    int
	MaxStreams  int
	IdleTimeout time.Duration
}

type PortRange struct {
	First int `json:"first"`
	Last  int `json:"last"`
}
type PeerAccess struct {
	Pin       string      `json:"pin,omitempty"`
	Addresses []string    `json:"addresses"`
	Ports     []PortRange `json:"ports"`
}

func validPin(pin string) bool {
	if len(pin) != sha256.Size*2 || strings.ToLower(pin) != pin {
		return false
	}
	_, err := hex.DecodeString(pin)
	return err == nil
}

func (c Config) Validate() error {
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil || host == "" || port == "" {
		return errors.New("Ray relay listen address must be host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("Ray relay listen port is invalid")
	}
	if len(c.Certificate.Certificate) == 0 || c.Certificate.PrivateKey == nil || c.ClientRoots == nil || len(c.Peers) < 2 {
		return errors.New("gateway certificate, client CA and at least two pinned peers are required")
	}
	for id, peer := range c.Peers {
		if strings.TrimSpace(id) != id || id == "" || !validPin(peer.Pin) || len(peer.Addresses) == 0 || len(peer.Ports) == 0 {
			return errors.New("peer identities and certificate pins must be valid")
		}
		for _, address := range peer.Addresses {
			if net.ParseIP(address) == nil || net.ParseIP(address).IsUnspecified() {
				return errors.New("peer address must be an explicit IP address")
			}
		}
		for _, ports := range peer.Ports {
			if ports.First < 1 || ports.Last > 65535 || ports.First > ports.Last {
				return errors.New("peer port range is invalid")
			}
		}
	}
	if c.MaxNodes < 2 || c.MaxNodes > 256 || c.MaxStreams < 1 || c.MaxStreams > 65536 || c.IdleTimeout < time.Second || c.IdleTimeout > 30*time.Minute {
		return errors.New("Ray relay limits are invalid")
	}
	return nil
}

type streamRequest struct {
	Target  string `json:"target"`
	Address string `json:"address"`
}

type peer struct {
	session *smux.Session
}

type Server struct {
	config  Config
	mu      sync.RWMutex
	peers   map[string]peer
	streams chan struct{}
}

func NewServer(config Config) (*Server, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Server{config: config, peers: make(map[string]peer), streams: make(chan struct{}, config.MaxStreams)}, nil
}

// Serve accepts node-scoped mTLS sessions. A certificate is bound to exactly
// one configured node identity and cannot register another peer's streams.
func (s *Server) Serve(ctx context.Context) error {
	listener, err := tls.Listen("tcp", s.config.Listen, &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: s.config.ClientRoots.Clone(), Certificates: []tls.Certificate{s.config.Certificate}})
	if err != nil {
		return fmt.Errorf("listen for Ray peers: %w", err)
	}
	defer listener.Close()
	defer s.closePeers()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
			s.closePeers()
		case <-done:
		}
	}()
	var handlers sync.WaitGroup
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				handlers.Wait()
				return nil
			}
			return fmt.Errorf("accept Ray peer: %w", err)
		}
		tlsConn, ok := conn.(*tls.Conn)
		if !ok {
			_ = conn.Close()
			continue
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			s.servePeer(ctx, tlsConn)
		}()
	}
}

func (s *Server) servePeer(ctx context.Context, conn *tls.Conn) {
	hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if err := conn.HandshakeContext(hctx); err != nil {
		cancel()
		_ = conn.Close()
		return
	}
	cancel()
	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		_ = conn.Close()
		return
	}
	cert := state.PeerCertificates[0]
	identity := cert.Subject.CommonName
	access, allowed := s.config.Peers[identity]
	got := sha256.Sum256(cert.Raw)
	if !allowed || subtle.ConstantTimeCompare([]byte(hex.EncodeToString(got[:])), []byte(access.Pin)) != 1 {
		_ = conn.Close()
		return
	}
	session, err := smux.Server(conn, muxConfig())
	if err != nil {
		_ = conn.Close()
		return
	}
	s.mu.Lock()
	if _, exists := s.peers[identity]; !exists && len(s.peers) >= s.config.MaxNodes {
		s.mu.Unlock()
		_ = session.Close()
		return
	}
	old := s.peers[identity]
	s.peers[identity] = peer{session: session}
	s.mu.Unlock()
	if old.session != nil {
		_ = old.session.Close()
	}
	defer func() {
		s.mu.Lock()
		if current := s.peers[identity]; current.session == session {
			delete(s.peers, identity)
		}
		s.mu.Unlock()
		_ = session.Close()
	}()
	var handlers sync.WaitGroup
	defer handlers.Wait()
	for {
		stream, err := session.AcceptStream()
		if err != nil {
			return
		}
		select {
		case s.streams <- struct{}{}:
		default:
			_ = stream.Close()
			continue
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			defer func() { <-s.streams; _ = stream.Close() }()
			s.route(ctx, identity, stream)
		}()
	}
}

func (s *Server) route(ctx context.Context, source string, incoming net.Conn) {
	_ = incoming.SetReadDeadline(time.Now().Add(5 * time.Second))
	request, err := readRequest(incoming)
	if err != nil || request.Target == source || request.Address == "" {
		return
	}
	access, exists := s.config.Peers[request.Target]
	if !exists || !access.allows(request.Address) {
		_, _ = incoming.Write([]byte{0})
		return
	}
	s.mu.RLock()
	target := s.peers[request.Target]
	s.mu.RUnlock()
	if target.session == nil || target.session.IsClosed() {
		_, _ = incoming.Write([]byte{0})
		return
	}
	stream, err := target.session.OpenStream()
	if err != nil {
		_, _ = incoming.Write([]byte{0})
		return
	}
	defer stream.Close()
	_ = stream.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(stream).Encode(request.Address); err != nil {
		_, _ = incoming.Write([]byte{0})
		return
	}
	var status [1]byte
	_ = stream.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(stream, status[:]); err != nil || status[0] != 1 {
		_, _ = incoming.Write([]byte{0})
		return
	}
	_ = incoming.SetDeadline(time.Time{})
	_ = stream.SetDeadline(time.Time{})
	_, _ = incoming.Write([]byte{1})
	relay(incoming, stream, s.config.IdleTimeout)
}

func readRequest(conn net.Conn) (streamRequest, error) {
	line := make([]byte, 0, 256)
	var one [1]byte
	for len(line) < maxRequestBytes {
		if _, err := io.ReadFull(conn, one[:]); err != nil {
			return streamRequest{}, errors.New("invalid Ray relay stream request")
		}
		line = append(line, one[0])
		if one[0] == '\n' {
			break
		}
	}
	if len(line) == maxRequestBytes && line[len(line)-1] != '\n' {
		return streamRequest{}, errors.New("invalid Ray relay stream request")
	}
	var req streamRequest
	d := json.NewDecoder(strings.NewReader(string(line)))
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil || req.Target == "" || req.Address == "" {
		return streamRequest{}, errors.New("invalid Ray relay stream request")
	}
	return req, nil
}

func (s *Server) closePeers() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, p := range s.peers {
		_ = p.session.Close()
		delete(s.peers, id)
	}
}

// Client maintains one authenticated multiplexed session from a node to A.
type Client struct {
	Address string
	// LocalAddress selects an existing source IP for the gateway connection only.
	// It never changes peer destinations, routes or the host's default address.
	LocalAddress string
	ServerName   string
	Roots        *x509.CertPool
	Certificate  tls.Certificate
	Pin          string
	NodeID       string
	Peers        map[string]PeerAccess
	MaxStreams   int
	IdleTimeout  time.Duration
	mu           sync.RWMutex
	session      *smux.Session
}

func (c *Client) Validate() error {
	if !validAddress(c.Address) || c.ServerName == "" || c.Roots == nil || len(c.Certificate.Certificate) == 0 || c.Certificate.PrivateKey == nil || !validPin(c.Pin) || c.NodeID == "" || len(c.Peers) < 2 || c.MaxStreams < 1 || c.MaxStreams > 65536 || c.IdleTimeout < time.Second || c.IdleTimeout > 30*time.Minute {
		return errors.New("Ray relay client configuration is invalid")
	}
	if c.LocalAddress != "" {
		ip := net.ParseIP(c.LocalAddress)
		if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
			return errors.New("Ray relay source address must be an explicit unicast IP without a port")
		}
	}
	seen := map[string]bool{}
	for id, peer := range c.Peers {
		if id == "" || strings.TrimSpace(id) != id || len(peer.Addresses) == 0 || len(peer.Ports) == 0 {
			return errors.New("Ray relay client peer access is invalid")
		}
		for _, address := range peer.Addresses {
			ip := net.ParseIP(address)
			if ip == nil || ip.IsUnspecified() || seen[ip.String()] {
				return errors.New("Ray relay client peer addresses must be unique IPs")
			}
			seen[ip.String()] = true
		}
		for _, ports := range peer.Ports {
			if ports.First < 1 || ports.Last > 65535 || ports.First > ports.Last {
				return errors.New("Ray relay client peer port range is invalid")
			}
		}
	}
	if _, ok := c.Peers[c.NodeID]; !ok {
		return errors.New("Ray relay client identity is not in the peer map")
	}
	return nil
}

// ServeConnector accepts the binary destination preamble used by the Linux
// shim and returns a short-lived loopback data port. The application's socket
// uses native connect semantics; remote setup happens after the data accept.
// This is a trusted-container boundary, not isolation from other local users.
func (c *Client) ServeConnector(ctx context.Context, listen string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := validateConnectorAddress(listen); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen for local Ray connector: %w", err)
	}
	defer listener.Close()
	limit := make(chan struct{}, c.MaxStreams)
	var connections sync.Mutex
	active := make(map[net.Conn]struct{})
	closeActive := func() {
		connections.Lock()
		defer connections.Unlock()
		for conn := range active {
			_ = conn.Close()
		}
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
			closeActive()
		case <-done:
		}
	}()
	var handlers sync.WaitGroup
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				handlers.Wait()
				return nil
			}
			return fmt.Errorf("accept local Ray connector: %w", err)
		}
		select {
		case limit <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		connections.Lock()
		if ctx.Err() != nil {
			connections.Unlock()
			<-limit
			_ = conn.Close()
			continue
		}
		active[conn] = struct{}{}
		connections.Unlock()
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			defer func() {
				<-limit
				_ = conn.Close()
				connections.Lock()
				delete(active, conn)
				connections.Unlock()
			}()
			c.serveLocal(ctx, conn)
		}()
	}
}

func validateConnectorAddress(listen string) error {
	host, port, err := net.SplitHostPort(listen)
	ip := net.ParseIP(host)
	n, portErr := strconv.Atoi(port)
	if err != nil || ip == nil || !ip.IsLoopback() || portErr != nil || n < 1 || n > 65535 {
		return errors.New("Ray process connector must bind to an explicit loopback IP and port")
	}
	return nil
}

func (c *Client) serveLocal(ctx context.Context, conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	var header [6]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return
	}
	address := net.JoinHostPort(net.IP(header[:4]).String(), strconv.Itoa(int(binary.BigEndian.Uint16(header[4:]))))
	target := ""
	for id, peer := range c.Peers {
		if id != c.NodeID && peer.allows(address) {
			if target != "" {
				_, _ = conn.Write([]byte{1})
				return
			}
			target = id
		}
	}
	if target == "" {
		_, _ = conn.Write([]byte{1})
		return
	}
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		_, _ = conn.Write([]byte{1})
		return
	}
	defer listener.Close()
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	_ = listener.SetDeadline(time.Now().Add(5 * time.Second))
	var response [3]byte
	binary.BigEndian.PutUint16(response[1:], uint16(listener.Addr().(*net.TCPAddr).Port))
	if _, err := conn.Write(response[:]); err != nil {
		return
	}
	// Close the control socket before waiting for data. No protocol bytes ever
	// enter the application socket, including on nonblocking connections.
	_ = conn.Close()
	data, err := listener.AcceptTCP()
	if err != nil {
		return
	}
	defer data.Close()
	closeData := context.AfterFunc(ctx, func() { _ = data.Close() })
	defer closeData()
	_ = listener.Close()
	stream, err := c.DialPeer(ctx, target, address)
	if err != nil {
		// TCP connect succeeded to the local proxy; remote failure is observed
		// as a reset during I/O, not as a fabricated SO_ERROR/connect result.
		_ = data.SetLinger(0)
		return
	}
	defer stream.Close()
	relay(data, stream, c.IdleTimeout)
}

// Run reconnects with bounded backoff and accepts peer streams, dialing only
// the address requested by another authenticated cluster member.
func (c *Client) Run(ctx context.Context) error {
	if err := c.Validate(); err != nil {
		return err
	}
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		conn, err := c.dial(ctx)
		if err == nil {
			session, sessionErr := smux.Client(conn, muxConfig())
			if sessionErr == nil {
				c.mu.Lock()
				c.session = session
				c.mu.Unlock()
				c.serve(ctx, session)
				c.mu.Lock()
				if c.session == session {
					c.session = nil
				}
				c.mu.Unlock()
			} else {
				_ = conn.Close()
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(started) > 30*time.Second {
			backoff = time.Second
		}
		if !sleep(ctx, backoff) {
			return nil
		}
		if backoff < 16*time.Second {
			backoff *= 2
		}
	}
	return nil
}

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	config := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: c.ServerName, RootCAs: c.Roots.Clone(), Certificates: []tls.Certificate{c.Certificate}, VerifyConnection: func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return errors.New("Ray gateway certificate missing")
		}
		got := sha256.Sum256(state.PeerCertificates[0].Raw)
		if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(got[:])), []byte(c.Pin)) != 1 {
			return errors.New("Ray gateway certificate pin mismatch")
		}
		return nil
	}}
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	if c.LocalAddress != "" {
		dialer.LocalAddr = &net.TCPAddr{IP: net.ParseIP(c.LocalAddress)}
	}
	return (&tls.Dialer{NetDialer: dialer, Config: config}).DialContext(dialCtx, "tcp", c.Address)
}

func (c *Client) serve(ctx context.Context, session *smux.Session) {
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
	limit := make(chan struct{}, c.MaxStreams)
	for {
		stream, err := session.AcceptStream()
		if err != nil {
			return
		}
		select {
		case limit <- struct{}{}:
		default:
			_ = stream.Close()
			continue
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			defer func() { <-limit; _ = stream.Close() }()
			c.serveTarget(ctx, stream)
		}()
	}
}

func (c *Client) serveTarget(ctx context.Context, stream net.Conn) {
	_ = stream.SetReadDeadline(time.Now().Add(5 * time.Second))
	var address string
	if err := json.NewDecoder(io.LimitReader(stream, maxRequestBytes)).Decode(&address); err != nil || !validAddress(address) {
		_, _ = stream.Write([]byte{0})
		return
	}
	if !c.Peers[c.NodeID].allows(address) {
		_, _ = stream.Write([]byte{0})
		return
	}
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	upstream, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", address)
	if err != nil {
		_, _ = stream.Write([]byte{0})
		return
	}
	defer upstream.Close()
	_ = stream.SetDeadline(time.Time{})
	_, _ = stream.Write([]byte{1})
	relay(stream, upstream, c.IdleTimeout)
}

// DialPeer opens an application TCP connection through A to one exact peer.
// The caller must derive target and address from the durable Ray assignment.
func (c *Client) DialPeer(ctx context.Context, target, address string) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.RLock()
	session := c.session
	c.mu.RUnlock()
	access, allowed := c.Peers[target]
	if target == "" || target == c.NodeID || !validAddress(address) || !allowed || !access.allows(address) || session == nil || session.IsClosed() {
		return nil, errors.New("invalid Ray peer target")
	}
	stream, err := session.OpenStream()
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stop()
	_ = stream.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(stream).Encode(streamRequest{Target: target, Address: address}); err != nil {
		_ = stream.Close()
		return nil, err
	}
	var status [1]byte
	if _, err := io.ReadFull(stream, status[:]); err != nil || status[0] != 1 {
		_ = stream.Close()
		return nil, errors.New("Ray peer is unavailable")
	}
	_ = stream.SetDeadline(time.Time{})
	return stream, nil
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

func validAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

func (p PeerAccess) allows(address string) bool {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	allowedAddress := false
	for _, candidate := range p.Addresses {
		if ip.Equal(net.ParseIP(candidate)) {
			allowedAddress = true
			break
		}
	}
	port, err := strconv.Atoi(portText)
	if err != nil || !allowedAddress {
		return false
	}
	for _, r := range p.Ports {
		if port >= r.First && port <= r.Last {
			return true
		}
	}
	return false
}

func sleep(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

type idleConn struct {
	net.Conn
	timeout time.Duration
}

func (c idleConn) CloseWrite() error {
	if half, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return half.CloseWrite()
	}
	return c.Conn.Close()
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
	results := make(chan error, 2)
	copyHalf := func(dst, src net.Conn) {
		_, err := io.Copy(idleConn{Conn: dst, timeout: idle}, idleConn{Conn: src, timeout: idle})
		if err == nil {
			if half, ok := dst.(interface{ CloseWrite() error }); ok {
				err = half.CloseWrite()
			}
		}
		results <- err
	}
	go copyHalf(left, right)
	go copyHalf(right, left)
	if err := <-results; err != nil {
		_ = left.Close()
		_ = right.Close()
	}
	<-results
	_ = left.Close()
	_ = right.Close()
}
