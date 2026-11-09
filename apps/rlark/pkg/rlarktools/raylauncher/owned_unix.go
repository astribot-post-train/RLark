//go:build unix

package raylauncher

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ownedStopGrace bounds how long a stopped process group may take to exit
// after SIGTERM before it is killed.
var ownedStopGrace = 30 * time.Second

// startOwned starts executable in a new process group. Cancelling ctx sends
// SIGTERM to that group only (Ray's --block stops its GCS, raylet and workers
// on SIGTERM), then SIGKILL after ownedStopGrace. It returns the wait function.
func startOwned(ctx context.Context, executable string, args ...string) func() error {
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return signalGroup(cmd, syscall.SIGTERM)
	}
	cmd.WaitDelay = ownedStopGrace
	if err := cmd.Start(); err != nil {
		return func() error { return err }
	}
	return func() error {
		err := cmd.Wait()
		// The group leader is gone; make sure nothing it started outlives it.
		_ = signalGroup(cmd, syscall.SIGKILL)
		return err
	}
}

func signalGroup(cmd *exec.Cmd, signal syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
