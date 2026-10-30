//go:build linux

package supervision

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/raw"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	"golang.org/x/sys/unix"
)

const treeGrace = 30 * time.Second
const treeCleanupTimeout = 30 * time.Second

type treeRequest struct {
	Record raw.Record `json:"record"`
}

type treeResult struct {
	TaskUID string `json:"taskUID"`
	Phase   string `json:"phase"`
	Cleaned bool   `json:"cleaned"`
}

// startTree runs a dedicated subreaper, never the multi-task service itself.
// Only the dedicated worker's kernel child relationships authorize signals.
func (s *Service) startTree(ctx context.Context, k string, r raw.Record, output *boundedLog, log *os.File) (raw.Record, error) {
	var files []*os.File
	pipe := func() (*os.File, *os.File, error) {
		a, b, err := os.Pipe()
		if err == nil {
			files = append(files, a, b)
		}
		return a, b, err
	}
	closeFiles := func() {
		for _, f := range files {
			_ = f.Close()
		}
	} // Includes ends already transferred/closed on error.
	input, send, err := pipe()
	if err != nil {
		_ = log.Close()
		return raw.Record{}, err
	}
	result, reply, err := pipe()
	if err != nil {
		closeFiles()
		_ = log.Close()
		return raw.Record{}, err
	}
	control, stop, err := pipe()
	if err != nil {
		closeFiles()
		_ = log.Close()
		return raw.Record{}, err
	}
	cmd := exec.Command(s.config.WorkerExecutable, "--process-tree-worker")
	cmd.Env = []string{}
	cmd.Dir = s.config.StateDirectory
	cmd.ExtraFiles = []*os.File{input, reply, control}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = output, output
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		closeFiles()
		_ = log.Close()
		r.Phase, r.Message, r.Cleaned = "Failed", "Raw process-tree worker failed to start", true
		persistErr := s.persist(r)
		if persistErr == nil {
			s.active = ""
		} else {
			r.Phase, r.Message, r.Cleaned = "Unknown", "Raw start failure could not be persisted", false
		}
		s.records[k] = r
		return r, errors.Join(err, persistErr)
	}
	// The child alone owns these pipe ends now.
	_ = input.Close()
	_ = reply.Close()
	_ = control.Close()
	s.process, s.control, s.done, s.stopping = cmd, stop, make(chan struct{}), false
	r.Phase = "Running"
	s.records[k] = r
	go func() {
		// Even a broken request pipe cannot authorize another execution: only a
		// valid worker outcome may release this pending/running record.
		_ = json.NewEncoder(send).Encode(treeRequest{Record: r})
		_ = send.Close()
	}()
	go s.reapTree(k, cmd, result, stop, log)
	if err := s.persist(r); err != nil {
		return raw.Record{}, err
	}
	return s.observe(ctx, r.Task)
}

func (s *Service) reapTree(k string, cmd *exec.Cmd, result, stop, log *os.File) {
	var outcome treeResult
	decodeErr := json.NewDecoder(io.LimitReader(result, 4096)).Decode(&outcome)
	_ = result.Close()
	waitErr := cmd.Wait()
	closeErr := log.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = stop.Close()
	r := s.records[k]
	r.Phase, r.Message, r.Cleaned = "Unknown", "Process-tree worker lost; cleanup requires container recovery", false
	validPhase := outcome.Phase == "Succeeded" || outcome.Phase == "Failed" || outcome.Phase == "Stopped"
	if decodeErr == nil && waitErr == nil && outcome.TaskUID == r.Task.ObjectUID && outcome.Cleaned && validPhase {
		r.Phase, r.Message, r.Cleaned = rt.InstancePhase(outcome.Phase), "", true
	}
	if closeErr != nil {
		r.Message = "Raw log could not be closed"
	}
	persistErr := s.persist(r)
	if persistErr != nil {
		r.Phase, r.Message, r.Cleaned = "Unknown", "Raw result could not be persisted", false
	}
	s.records[k], s.process, s.control = r, nil, nil
	if r.Cleaned && persistErr == nil {
		s.active = ""
	}
	close(s.done)
}

// RunProcessTreeWorker is the raw-supervisor internal child entry point. It must
// run in a dedicated process with no unrelated children. FDs 3/4/5 carry intent,
// outcome and stop requests; none are inherited by the training executable.
// A root workload is trusted: this is lifecycle supervision, not a sandbox.
func RunProcessTreeWorker() error {
	if os.Geteuid() != 0 {
		return errors.New("process-tree worker requires root")
	}
	for _, fd := range []int{3, 4, 5} {
		unix.CloseOnExec(fd)
	}
	input := os.NewFile(3, "intent")
	result := os.NewFile(4, "result")
	control := os.NewFile(5, "control")
	defer func() { _ = input.Close(); _ = result.Close(); _ = control.Close() }()
	var req treeRequest
	decoder := json.NewDecoder(io.LimitReader(input, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return errors.New("invalid process-tree intent")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("multiple process-tree intents")
	}
	if req.Record.Task.ObjectUID == "" || req.Record.Task.Logical.Validate() != nil {
		return errors.New("process-tree Task identity required")
	}
	if err := req.Record.Spec.Validate(); err != nil {
		return err
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("enable execution subreaper: %w", err)
	}
	// Verify that /proc reports this PID and that the required pidfd operations
	// work before allowing any workload code to execute.
	self, err := os.Readlink("/proc/self")
	if err != nil || self != strconv.Itoa(os.Getpid()) {
		return errors.New("process-tree requires matching proc and PID namespaces")
	}
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return fmt.Errorf("process-tree requires pidfd: %w", err)
	}
	err = unix.PidfdSendSignal(fd, 0, nil, 0)
	_ = unix.Close(fd)
	if err != nil {
		return err
	}
	outcome := treeResult{TaskUID: req.Record.Task.ObjectUID, Phase: "Failed"}
	cmd := exec.Command(req.Record.Spec.Argv[0], req.Record.Spec.Argv[1:]...)
	cmd.Env, cmd.Dir = append([]string{}, req.Record.Spec.Env...), req.Record.Spec.Directory
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		outcome.Cleaned = true // exec.Start failed before workload execution.
		return json.NewEncoder(result).Encode(outcome)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	stopped := make(chan struct{}, 1)
	go func() {
		line, err := bufio.NewReader(control).ReadString('\n')
		if err == nil && line == "stop\n" {
			stopped <- struct{}{}
		}
		// EOF from a lost service is not permission to kill or release anything.
	}()
	select {
	case err := <-exited:
		if err == nil {
			outcome.Phase = "Succeeded"
		}
	case <-stopped:
		outcome.Phase = "Stopped"
		// Go's Process handle uses pidfd on supported Linux kernels and never
		// signals a reused PID after Wait has completed.
		_ = cmd.Process.Signal(syscall.SIGTERM)
		timer := time.NewTimer(treeGrace)
		select {
		case exitErr := <-exited:
			_ = exitErr // Stop was requested; exit status does not change the stopped outcome.
		case <-timer.C:
			_ = cmd.Process.Kill()
			// Do not wait forever for an uninterruptible main process.
			select {
			case exitErr := <-exited:
				_ = exitErr // Stop was requested; exit status does not change the stopped outcome.
			case <-time.After(treeCleanupTimeout):
				return errors.New("main process termination unconfirmed")
			}
		}
		timer.Stop()
	}
	// The main child has already been waited. Remaining children are solely
	// descendants adopted from this execution, including nested subreapers.
	if err := cleanupTree(time.Now().Add(treeCleanupTimeout)); err != nil {
		return err
	}
	outcome.Cleaned = true
	return json.NewEncoder(result).Encode(outcome)
}

func cleanupTree(deadline time.Time) error {
	for time.Now().Before(deadline) {
		// ECHILD is the final proof; a transiently empty /proc listing is not.
		var status unix.WaitStatus
		pid, err := unix.Wait4(-1, &status, unix.WNOHANG, nil)
		if errors.Is(err, unix.ECHILD) {
			return nil
		}
		if err != nil && !errors.Is(err, unix.EINTR) {
			return err
		}
		if pid > 0 {
			continue
		}
		children, err := directChildren()
		if err != nil {
			return err
		}
		for _, child := range children {
			fd, err := unix.PidfdOpen(child, 0)
			if errors.Is(err, unix.ESRCH) {
				continue
			}
			if err != nil {
				return err
			}
			// Prove the opened process is still our child. If its numeric PID
			// was recycled, waitid returns ECHILD and we do not signal it.
			var info unix.Siginfo
			err = unix.Waitid(unix.P_PIDFD, fd, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
			if err == nil {
				err = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
			}
			_ = unix.Close(fd)
			if err != nil && !errors.Is(err, unix.ECHILD) && !errors.Is(err, unix.ESRCH) {
				return err
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("process-tree cleanup deadline exceeded")
}

func directChildren() ([]int, error) {
	threads, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return nil, err
	}
	var children []int
	for _, thread := range threads {
		data, err := os.ReadFile(filepath.Join("/proc/self/task", thread.Name(), "children"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		} // Go runtime threads can exit.
		if err != nil {
			return nil, err
		}
		for _, value := range strings.Fields(string(data)) {
			pid, err := strconv.Atoi(value)
			if err != nil || pid <= 0 {
				return nil, errors.New("invalid child process identity")
			}
			children = append(children, pid)
		}
	}
	return children, nil
}
