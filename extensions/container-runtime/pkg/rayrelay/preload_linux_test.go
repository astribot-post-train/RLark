//go:build linux

package rayrelay

import (
	"context"
	"encoding/binary"
	"encoding/pem"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func buildPreloadClient(t *testing.T) (string, string) {
	t.Helper()
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("process interception tests require a Linux C compiler")
	}
	dir := t.TempDir()
	shim, client := filepath.Join(dir, "connect.so"), filepath.Join(dir, "client")
	for _, args := range [][]string{
		{"-Wall", "-Wextra", "-Werror", "-fPIC", "-shared", "-o", shim, "preload/connect.c", "-ldl", "-pthread"},
		{"-Wall", "-Wextra", "-Werror", "-o", client, "testdata/connect_client.c"},
	} {
		if output, err := exec.Command(cc, args...).CombinedOutput(); err != nil {
			t.Fatalf("compile process test: %v: %s", err, output)
		}
	}
	return shim, client
}

func TestPreloadRejectsInvalidPeerLists(t *testing.T) {
	shim, client := buildPreloadClient(t)
	for _, ips := range []string{"", ",192.0.2.1", "192.0.2.1,", "192.0.2.1,,192.0.2.2", "192.0.2.1,192.0.2.1", "192.0.2.1,garbage", "0.0.0.0", "::1", "192.0.2.1, 192.0.2.2"} {
		t.Run(ips, func(t *testing.T) {
			runPreloadClient(t, shim, client, "192.0.2.1", "20001", "1", "invalid", []string{"RLARK_MUX_REMOTE_IPS=" + ips, "RLARK_MUX_PORT_MIN=20000", "RLARK_MUX_PORT_MAX=20010", "RLARK_MUX_LOCAL_PORT=1"})
		})
	}
	addresses := make([]string, 65)
	for i := range addresses {
		addresses[i] = "192.0.2." + strconv.Itoa(i+1)
	}
	runPreloadClient(t, shim, client, "192.0.2.1", "20001", "0", "invalid", []string{"RLARK_MUX_REMOTE_IPS=" + strings.Join(addresses, ","), "RLARK_MUX_PORT_MIN=20000", "RLARK_MUX_PORT_MAX=20010", "RLARK_MUX_LOCAL_PORT=1"})
	runPreloadClient(t, shim, client, "192.0.2.1", "20001", "0", "invalid", []string{"RLARK_MUX_REMOTE_IP=192.0.2.1", "RLARK_MUX_REMOTE_IPS=192.0.2.2", "RLARK_MUX_PORT_MIN=20000", "RLARK_MUX_PORT_MAX=20010", "RLARK_MUX_LOCAL_PORT=1"})
}

func runPreloadClient(t *testing.T, shim, client, address, port, nonblocking, expected string, env []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, client, address, port, nonblocking, expected)
	// Explicit environment; never inherit credentials, proxy or other preload settings.
	cmd.Env = append([]string{"LD_PRELOAD=" + shim}, env...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("preloaded process: %v: %s", err, output)
	}
}

func TestPreloadProcessConnect(t *testing.T) {
	shim, client := buildPreloadClient(t)
	for _, mode := range []string{"ipv4", "mapped-ipv6", "multi-first", "multi-last", "denied", "eof", "timeout", "nonblocking", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
			env := []string{"RLARK_MUX_REMOTE_IP=192.0.2.10", "RLARK_MUX_PORT_MIN=20000", "RLARK_MUX_PORT_MAX=20010", "RLARK_MUX_LOCAL_PORT=" + port}
			address, expected, nonblocking := "192.0.2.10", "success", "0"
			switch mode {
			case "multi-first", "multi-last":
				env[0] = "RLARK_MUX_REMOTE_IPS=192.0.2.10,192.0.2.11"
				if mode == "multi-last" {
					address = "192.0.2.11"
				}
			case "mapped-ipv6":
				address = "::ffff:192.0.2.10"
			case "denied":
				expected = "refused"
			case "eof":
				expected = "reset"
			case "timeout":
				expected = "timeout"
			case "nonblocking":
				nonblocking = "1"
			case "invalid":
				env = []string{"RLARK_MUX_REMOTE_IP=invalid"}
				expected = "invalid"
			}
			done := make(chan error, 1)
			if mode != "invalid" {
				go func() {
					control, err := listener.Accept()
					if err != nil {
						done <- err
						return
					}
					defer control.Close()
					_ = control.SetDeadline(time.Now().Add(8 * time.Second))
					var header [6]byte
					if _, err := io.ReadFull(control, header[:]); err != nil {
						done <- err
						return
					}
					if !net.IP(header[:4]).Equal(net.ParseIP(address)) || binary.BigEndian.Uint16(header[4:]) != 20001 {
						done <- io.ErrUnexpectedEOF
						return
					}
					switch mode {
					case "eof":
						done <- nil
						return
					case "timeout":
						var b [3]byte
						_, err := control.Read(b[:])
						if err == io.EOF {
							err = nil
						}
						done <- err
						return
					case "denied":
						_, err := control.Write([]byte{1})
						done <- err
						return
					}
					dataListener, err := net.Listen("tcp4", "127.0.0.1:0")
					if err != nil {
						done <- err
						return
					}
					defer dataListener.Close()
					dataPort := dataListener.Addr().(*net.TCPAddr).Port
					var response [3]byte
					response[1] = byte(dataPort >> 8)
					response[2] = byte(dataPort)
					if _, err := control.Write(response[:]); err != nil {
						done <- err
						return
					}
					data, err := dataListener.Accept()
					if err != nil {
						done <- err
						return
					}
					defer data.Close()
					var payload [4]byte
					if _, err := io.ReadFull(data, payload[:]); err != nil {
						done <- err
						return
					}
					_, err = data.Write(payload[:])
					done <- err
				}()
			}
			runPreloadClient(t, shim, client, address, "20001", nonblocking, expected, env)
			if mode != "invalid" {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestPreloadUnmatchedAndUnconfiguredPassThrough(t *testing.T) {
	shim, client := buildPreloadClient(t)
	for _, env := range [][]string{nil, {"RLARK_MUX_REMOTE_IP=192.0.2.10", "RLARK_MUX_PORT_MIN=1", "RLARK_MUX_PORT_MAX=65535", "RLARK_MUX_LOCAL_PORT=1"}} {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		done := make(chan error, 1)
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				done <- err
				return
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			var payload [4]byte
			if _, err := io.ReadFull(conn, payload[:]); err != nil {
				done <- err
				return
			}
			_, err = conn.Write(payload[:])
			done <- err
		}()
		runPreloadClient(t, shim, client, "127.0.0.1", strconv.Itoa(listener.Addr().(*net.TCPAddr).Port), "0", "success", env)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

// Exercise the shipped credential/config loaders, service lifecycle, TLS mux,
// local connector and compiled C shim together, in both directions.
func TestConfiguredServicesWithPreloadedNonblockingProcesses(t *testing.T) {
	shim, executable := buildPreloadClient(t)
	ca, _, ids := testPKI(t)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	freeAddress := func() string {
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := l.Addr().String()
		_ = l.Close()
		return address
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	services := make(chan error, 4)
	peers := make(map[string]PeerAccess)
	for i, id := range []string{"node-a", "node-b", "node-c"} {
		ip := "127.0.0." + strconv.Itoa(i+2)
		listener, err := net.Listen("tcp4", ip+":0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		port := listener.Addr().(*net.TCPAddr).Port
		peers[id] = PeerAccess{Pin: pin(ids[id].leaf), Addresses: []string{ip}, Ports: []PortRange{{First: port, Last: port}}}
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				go func() {
					defer conn.Close()
					_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
					var b [4]byte
					if _, err := io.ReadFull(conn, b[:]); err == nil {
						_, _ = conn.Write(b[:])
					}
				}()
			}
		}()
	}
	cert, key := writeTestCredentials(t, ids["gateway"])
	gatewayAddress := freeAddress()
	gateway, err := LoadGateway(writeTestConfig(t, ServiceConfig{Mode: "gateway", Listen: gatewayAddress, Certificate: cert, PrivateKey: key, CA: caPath, Peers: peers, MaxNodes: 4, MaxStreams: 8, IdleSeconds: 30}))
	if err != nil {
		t.Fatal(err)
	}
	go func() { services <- gateway.Serve(ctx) }()
	connectors := make(map[string]string)
	for _, id := range []string{"node-a", "node-b", "node-c"} {
		cert, key := writeTestCredentials(t, ids[id])
		address := freeAddress()
		connectors[id] = address
		peer, err := LoadPeer(writeTestConfig(t, ServiceConfig{Mode: "peer", Listen: address, Certificate: cert, PrivateKey: key, CA: caPath, Peers: peers, GatewayAddress: gatewayAddress, ServerName: "localhost", GatewayPin: pin(ids["gateway"].leaf), NodeID: id, MaxStreams: 8, IdleSeconds: 30}))
		if err != nil {
			t.Fatal(err)
		}
		go func() { services <- peer.Run(ctx) }()
	}
	waitFor(t, 5*time.Second, func() bool { return len(gateway.connectedPeers()) == 3 })
	for source := range peers {
		_, connectorPort, _ := net.SplitHostPort(connectors[source])
		var remote []string
		for target, access := range peers {
			if source != target {
				remote = append(remote, access.Addresses...)
			}
		}
		sort.Strings(remote)
		for target, access := range peers {
			if target == source {
				continue
			}
			port := strconv.Itoa(access.Ports[0].First)
			for _, nonblocking := range []string{"0", "1"} {
				runPreloadClient(t, shim, executable, access.Addresses[0], port, nonblocking, "success", []string{"RLARK_MUX_REMOTE_IPS=" + strings.Join(remote, ","), "RLARK_MUX_PORT_MIN=1024", "RLARK_MUX_PORT_MAX=65535", "RLARK_MUX_LOCAL_PORT=" + connectorPort})
			}
		}
	}
	cancel()
	for i := 0; i < 4; i++ {
		select {
		case err := <-services:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("configured service did not stop")
		}
	}
}
