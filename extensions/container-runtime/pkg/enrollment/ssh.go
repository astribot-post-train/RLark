// Package enrollment implements opt-in SSH bootstrap of existing Notebook nodes.
// SSH carries installation/control only, never Ray inference traffic.
package enrollment

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/core/api/v1alpha1"
)

// Target uses the gateway's local worker identity; the server key stays on the server.
type Target struct {
	Gateway ext.EnrollmentNode
	Node    ext.EnrollmentNode
}

// Runner executes fixed installer programs with bounded, cancellable transport.
type Runner interface {
	Run(context.Context, Target, string, io.Reader) ([]byte, error)
}

// SSH uses strict preconfigured host trust on both hops. It never enables agent forwarding.
type SSH struct {
	IdentityFile, KnownHosts, WorkerIdentityFile, WorkerKnownHosts string
	// ControlDirectory optionally reuses installation-only SSH connections.
	// It must be private and specific to this trusted enrollment profile.
	ControlDirectory string
}

func (s SSH) validateControlDirectory() error {
	if s.ControlDirectory == "" {
		return nil
	}
	info, err := os.Lstat(s.ControlDirectory)
	resolved, resolveErr := filepath.EvalSymlinks(s.ControlDirectory)
	if err != nil || resolveErr != nil || !filepath.IsAbs(s.ControlDirectory) || resolved != s.ControlDirectory || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("SSH control directory must be a private canonical directory without symlinks")
	}
	// OpenSSH's Unix-domain control socket path is limited to 107 bytes on Linux.
	if len(filepath.Join(s.ControlDirectory, strings.Repeat("x", 40))) > 107 {
		return errors.New("SSH control directory is too long for a portable control socket")
	}
	return nil
}

func quote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
func (s SSH) arguments(t Target, command string) []string {
	args := []string{"-F", "/dev/null", "-T", "-oBatchMode=yes", "-oIdentitiesOnly=yes", "-oStrictHostKeyChecking=yes", "-oConnectTimeout=10", "-oConnectionAttempts=1", "-oServerAliveInterval=10", "-oServerAliveCountMax=2", "-oForwardAgent=no", "-oUserKnownHostsFile=" + s.KnownHosts, "-i", s.IdentityFile, "-p", strconv.Itoa(int(t.Gateway.Port)), "root@" + t.Gateway.Address}
	if s.ControlDirectory != "" {
		args = append([]string{"-oControlMaster=auto", "-oControlPersist=60", "-oControlPath=" + filepath.Join(s.ControlDirectory, "%C")}, args...)
	}
	if t.Node.Role == ext.NodeRoleWorker {
		nested := []string{"ssh", "-F", "/dev/null", "-T", "-oBatchMode=yes", "-oIdentitiesOnly=yes", "-oStrictHostKeyChecking=yes", "-oConnectTimeout=10", "-oConnectionAttempts=1", "-oForwardAgent=no", "-oUserKnownHostsFile=" + s.WorkerKnownHosts, "-i", s.WorkerIdentityFile, "-p", strconv.Itoa(int(t.Node.Port)), "root@" + t.Node.Address, command}
		words := make([]string, len(nested))
		for i, v := range nested {
			words[i] = quote(v)
		}
		command = strings.Join(words, " ")
	}
	return append(args, command)
}

type cappedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, errors.New("SSH output exceeded bound")
	}
	return b.Buffer.Write(p)
}
func (s SSH) Run(ctx context.Context, t Target, command string, input io.Reader) ([]byte, error) {
	if err := s.validateControlDirectory(); err != nil {
		return nil, err
	}
	if net.ParseIP(t.Gateway.Address) == nil || net.ParseIP(t.Node.Address) == nil || s.IdentityFile == "" || s.KnownHosts == "" || s.WorkerIdentityFile == "" || s.WorkerKnownHosts == "" {
		return nil, errors.New("SSH identity and strict host trust required")
	}
	cmd := exec.CommandContext(ctx, "ssh", s.arguments(t, command)...)
	cmd.Stdin = input
	output := &cappedBuffer{limit: 64 << 10}
	cmd.Stdout = output
	// Do not return arbitrary remote stderr (it may contain credentials/config).
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("SSH bootstrap failed: %w", err)
	}
	return output.Bytes(), nil
}
