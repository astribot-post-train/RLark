package rayrelay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"net"
	"strconv"
	"testing"
	"time"
)

type testIdentity struct {
	certificate tls.Certificate
	leaf        *x509.Certificate
}

func newTestIdentity(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, name string, server bool) testIdentity {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	usage := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	if server {
		usage = append(usage, x509.ExtKeyUsageServerAuth)
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usage, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return testIdentity{certificate: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, leaf: leaf}
}

func testPKI(t *testing.T) (tls.Certificate, *x509.CertPool, map[string]testIdentity) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Ray relay test CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	ids := map[string]testIdentity{
		"gateway": newTestIdentity(t, ca, caKey, "gateway", true),
		"node-a":  newTestIdentity(t, ca, caKey, "node-a", false),
		"node-b":  newTestIdentity(t, ca, caKey, "node-b", false),
		"node-c":  newTestIdentity(t, ca, caKey, "node-c", false),
		"rogue":   newTestIdentity(t, ca, caKey, "rogue", false),
	}
	return tls.Certificate{Certificate: [][]byte{caDER}, PrivateKey: caKey}, roots, ids
}

func pin(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

func TestGatewayDialBindsSourceAndRecoversFromUnavailableAddress(t *testing.T) {
	_, roots, ids := testPKI(t)
	listener, err := tls.Listen("tcp4", "127.0.0.1:0", &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{ids["gateway"].certificate},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	observed := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if err := conn.(*tls.Conn).Handshake(); err == nil {
			host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
			observed <- host
		}
	}()
	c := &Client{Address: listener.Addr().String(), LocalAddress: "192.0.2.123", ServerName: "localhost", Roots: roots, Certificate: ids["node-a"].certificate, Pin: pin(ids["gateway"].leaf)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if conn, err := c.dial(ctx); err == nil {
		_ = conn.Close()
		t.Fatal("unavailable source address silently fell back to default egress")
	}
	c.LocalAddress = "127.0.0.2"
	conn, err := c.dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	select {
	case source := <-observed:
		if source != c.LocalAddress {
			t.Fatalf("source = %s, want %s", source, c.LocalAddress)
		}
	case <-ctx.Done():
		t.Fatal("gateway did not observe bound source")
	}
}

func TestAuthenticatedPeerStreamAndPortPolicy(t *testing.T) {
	_, roots, ids := testPKI(t)
	service, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	servicePort := service.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			conn, err := service.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 32)
				n, _ := conn.Read(buf)
				_, _ = conn.Write(buf[:n])
			}()
		}
	}()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()

	serverCert := ids["gateway"].certificate
	server, err := NewServer(Config{
		Listen:      address,
		Certificate: serverCert,
		ClientRoots: roots,
		Peers: map[string]PeerAccess{
			"node-a": {Pin: pin(ids["node-a"].leaf), Addresses: []string{"127.0.0.2"}, Ports: []PortRange{{First: servicePort, Last: servicePort}}},
			"node-b": {Pin: pin(ids["node-b"].leaf), Addresses: []string{"127.0.0.1"}, Ports: []PortRange{{First: servicePort, Last: servicePort}}},
		},
		MaxNodes: 4, MaxStreams: 8, IdleTimeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	serverAddress := address
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.Serve(ctx) }()

	access := map[string]PeerAccess{
		"node-a": {Addresses: []string{"127.0.0.2"}, Ports: []PortRange{{First: servicePort, Last: servicePort}}},
		"node-b": {Addresses: []string{"127.0.0.1"}, Ports: []PortRange{{First: servicePort, Last: servicePort}}},
	}
	newClient := func(id string) *Client {
		client := &Client{Address: serverAddress, ServerName: "localhost", Roots: roots, Certificate: ids[id].certificate, Pin: pin(serverCert.Leaf), NodeID: id, Peers: access, MaxStreams: 8, IdleTimeout: time.Minute}
		go func() { _ = client.Run(ctx) }()
		return client
	}
	clientA := newClient("node-a")
	clientB := newClient("node-b")
	waitFor(t, 5*time.Second, func() bool {
		clientA.mu.RLock()
		defer clientA.mu.RUnlock()
		return clientA.session != nil && !clientA.session.IsClosed() && len(server.connectedPeers()) == 2
	})

	stream, err := clientA.DialPeer(ctx, "node-b", "127.0.0.1:"+strconv.Itoa(servicePort))
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.SetDeadline(time.Now().Add(time.Second))
	if _, err := stream.Write([]byte("ray-stream")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len("ray-stream"))
	if _, err := stream.Read(buf); err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	if string(buf) != "ray-stream" {
		t.Fatalf("echo mismatch: %q", buf)
	}

	connector, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	connectorAddress := connector.Addr().String()
	_ = connector.Close()
	connectorDone := make(chan error, 1)
	go func() { connectorDone <- clientA.ServeConnector(ctx, connectorAddress) }()
	var local net.Conn
	waitFor(t, time.Second, func() bool {
		var dialErr error
		local, dialErr = net.DialTimeout("tcp", connectorAddress, 20*time.Millisecond)
		return dialErr == nil
	})
	defer local.Close()
	var header [6]byte
	copy(header[:4], net.ParseIP("127.0.0.1").To4())
	binary.BigEndian.PutUint16(header[4:], uint16(servicePort))
	if _, err := local.Write(header[:]); err != nil {
		t.Fatal(err)
	}
	var response [3]byte
	if _, err := io.ReadFull(local, response[:]); err != nil || response[0] != 0 {
		t.Fatalf("connector rejected allowed peer: ack=%d err=%v", response[0], err)
	}
	_ = local.Close()
	local, err = net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(binary.BigEndian.Uint16(response[1:])))), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	if _, err := local.Write([]byte("connector-stream")); err != nil {
		t.Fatal(err)
	}
	connectorResponse := make([]byte, len("connector-stream"))
	if _, err := io.ReadFull(local, connectorResponse); err != nil {
		t.Fatal(err)
	}
	if string(connectorResponse) != "connector-stream" {
		t.Fatalf("connector echo mismatch: %q", connectorResponse)
	}
	denied, err := net.DialTimeout("tcp", connectorAddress, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer denied.Close()
	copy(header[:4], net.ParseIP("127.0.0.9").To4())
	if _, err := denied.Write(header[:]); err != nil {
		t.Fatal(err)
	}
	var ack [1]byte
	if _, err := io.ReadFull(denied, ack[:]); err != nil || ack[0] != 1 {
		t.Fatalf("connector accepted an unassigned peer: ack=%d err=%v", ack[0], err)
	}
	if _, err := clientA.DialPeer(ctx, "node-b", "127.0.0.1:1"); err == nil {
		t.Fatal("unassigned target port was accepted")
	}
	if _, err := clientA.DialPeer(ctx, "rogue", "127.0.0.1:"+strconv.Itoa(servicePort)); err == nil {
		t.Fatal("unregistered peer identity was accepted")
	}
	cancel()
	select {
	case err := <-connectorDone:
		if err != nil {
			t.Fatalf("connector did not stop cleanly: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("connector did not stop after cancellation")
	}
	_ = clientB
}

func (s *Server) connectedPeers() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.peers))
	for id := range s.peers {
		ids = append(ids, id)
	}
	return ids
}

func waitFor(t *testing.T, timeout time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become ready before timeout")
}

func TestPeerAccessRequiresExactAddressAndPort(t *testing.T) {
	access := PeerAccess{Addresses: []string{"10.0.0.2"}, Ports: []PortRange{{First: 20000, Last: 20010}}}
	for _, address := range []string{"10.0.0.2:20000", "10.0.0.2:20010"} {
		if !access.allows(address) {
			t.Errorf("expected %s to be allowed", address)
		}
	}
	for _, address := range []string{"10.0.0.3:20000", "10.0.0.2:19999", "10.0.0.2:20011", "example.com:20000", "0.0.0.0:20000"} {
		if access.allows(address) {
			t.Errorf("expected %s to be denied", address)
		}
	}
}

func TestConnectorCancellationClosesOrphanAndPendingData(t *testing.T) {
	_, roots, ids := testPKI(t)
	for _, connectData := range []bool{false, true} {
		t.Run(strconv.FormatBool(connectData), func(t *testing.T) {
			client := &Client{Address: "127.0.0.1:1", ServerName: "localhost", Roots: roots, Certificate: ids["node-a"].certificate, Pin: pin(ids["gateway"].leaf), NodeID: "node-a", MaxStreams: 1, IdleTimeout: time.Minute,
				Peers: map[string]PeerAccess{"node-a": {Addresses: []string{"192.0.2.1"}, Ports: []PortRange{{First: 20000, Last: 20010}}}, "node-b": {Addresses: []string{"192.0.2.2"}, Ports: []PortRange{{First: 20000, Last: 20010}}}}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			control, shim := net.Pipe()
			defer shim.Close()
			done := make(chan struct{})
			go func() { defer close(done); defer control.Close(); client.serveLocal(ctx, control) }()
			_ = shim.SetDeadline(time.Now().Add(time.Second))
			var header [6]byte
			copy(header[:4], net.ParseIP("192.0.2.2").To4())
			binary.BigEndian.PutUint16(header[4:], 20001)
			if _, err := shim.Write(header[:]); err != nil {
				t.Fatal(err)
			}
			var response [3]byte
			if _, err := io.ReadFull(shim, response[:]); err != nil {
				t.Fatal(err)
			}
			address := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(binary.BigEndian.Uint16(response[1:]))))
			if connectData {
				data, err := net.DialTimeout("tcp4", address, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer data.Close()
				_ = data.SetDeadline(time.Now().Add(time.Second))
				// Without a gateway session, local accept must not imply remote success.
				var b [1]byte
				if _, err := data.Read(b[:]); err == nil {
					t.Fatal("unavailable remote did not fail")
				} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
					t.Fatal("remote failure hung until deadline")
				}
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("orphan data listener survived cancellation")
			}
			if conn, err := net.DialTimeout("tcp4", address, 100*time.Millisecond); err == nil {
				conn.Close()
				t.Fatal("one-shot data listener remains open")
			}
			if _, err := client.DialPeer(ctx, "node-b", "192.0.2.2:20001"); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
		})
	}
}
