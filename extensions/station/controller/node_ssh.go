package controller

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// NodeSSHConfig is supplied from a short-lived operation credential, not status.
type NodeSSHConfig struct {
	Address, User, HostKeyFingerprint string
	Password                          []byte
	PrivateKey                        []byte
	Timeout                           time.Duration
}

// RunNodeSudoScript sends the sudo password and script over stdin. The script
// is supplied by trusted code, never by an API request. Passwordless sudo must
// be selected explicitly so sudo cannot consume the first line of the script.
func RunNodeSudoScript(ctx context.Context, client *ssh.Client, script string, password []byte) ([]byte, error) {
	command, input, err := nodeSudoInput(script, password)
	if err != nil {
		return nil, err
	}
	defer clear(input)
	return RunNodeSSHCommand(ctx, client, command, input)
}

func nodeSudoInput(script string, password []byte) (string, []byte, error) {
	if script == "" {
		return "", nil, errors.New("sudo script is required")
	}
	if bytes.ContainsAny(password, "\r\n\x00") {
		return "", nil, errors.New("sudo password contains unsupported characters")
	}
	var input bytes.Buffer
	command := "sudo -n /bin/sh -s"
	if password != nil {
		if len(password) == 0 {
			return "", nil, errors.New("sudo password is empty")
		}
		command = "sudo -k -S -p '' /bin/sh -s"
		input.Write(password)
		input.WriteByte('\n')
	}
	input.WriteString(script)
	if !strings.HasSuffix(script, "\n") {
		input.WriteByte('\n')
	}
	return command, input.Bytes(), nil
}

const maxNodeCommandOutput = 64 << 10

// RunNodeSSHCommand executes a caller-owned command without exposing remote
// stderr or stdin in errors. Cancellation closes the session.
func RunNodeSSHCommand(ctx context.Context, client *ssh.Client, command string, stdin []byte) ([]byte, error) {
	if client == nil || command == "" {
		return nil, errors.New("SSH client and command are required")
	}
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("create SSH session: %w", err)
	}
	defer session.Close()
	var output boundedNodeOutput
	session.Stdout = &output
	session.Stderr = io.Discard
	if stdin != nil {
		session.Stdin = bytes.NewReader(stdin)
	}
	stop := context.AfterFunc(ctx, func() { _ = session.Close() })
	defer stop()
	if err := session.Run(command); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("remote command failed")
	}
	if output.truncated {
		return nil, errors.New("remote command output exceeded limit")
	}
	return output.data.Bytes(), nil
}

type boundedNodeOutput struct {
	data      bytes.Buffer
	truncated bool
}

func (w *boundedNodeOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := maxNodeCommandOutput - w.data.Len()
	if remaining < len(p) {
		w.truncated = true
		p = p[:remaining]
	}
	_, _ = w.data.Write(p)
	return n, nil
}

// DialNodeSSH pins the host key before authentication and honors cancellation
// during both TCP connect and SSH handshake. It never logs credentials.
func DialNodeSSH(ctx context.Context, cfg NodeSSHConfig) (*ssh.Client, error) {
	if cfg.Address == "" || cfg.User == "" || cfg.HostKeyFingerprint == "" {
		return nil, errors.New("SSH address, user and verified host fingerprint are required")
	}
	if (len(cfg.Password) == 0) == (len(cfg.PrivateKey) == 0) {
		return nil, errors.New("exactly one SSH authentication method is required")
	}
	var auth ssh.AuthMethod
	if len(cfg.PrivateKey) != 0 {
		signer, err := ssh.ParsePrivateKey(cfg.PrivateKey)
		if err != nil {
			return nil, errors.New("invalid SSH private key")
		}
		auth = ssh.PublicKeys(signer)
	} else {
		auth = ssh.Password(string(cfg.Password))
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("connect SSH: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	config := &ssh.ClientConfig{User: cfg.User, Auth: []ssh.AuthMethod{auth}, HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
		actual := ssh.FingerprintSHA256(key)
		if subtle.ConstantTimeCompare([]byte(actual), []byte(cfg.HostKeyFingerprint)) != 1 {
			return fmt.Errorf("SSH host key fingerprint mismatch: received %s", actual)
		}
		return nil
	}}
	transport, channels, requests, err := ssh.NewClientConn(conn, cfg.Address, config)
	if err != nil {
		stop()
		_ = conn.Close()
		return nil, fmt.Errorf("SSH handshake: %w", err)
	}
	if !stop() {
		_ = transport.Close()
		return nil, ctx.Err()
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(transport, channels, requests), nil
}
