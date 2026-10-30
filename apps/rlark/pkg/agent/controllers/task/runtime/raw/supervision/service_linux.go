//go:build linux

// Package supervision owns Raw process effects independently of the Agent.
// An exclusive state-directory lock prevents two supervisors from using a node.
package supervision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rlinf/rlark/apps/rlark/pkg/agent/controllers/task/runtime/raw"
	"github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/fault"
	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	"golang.org/x/sys/unix"
)

// Config names private durable state and operator-authorized workload paths.
// StateDirectory must survive Agent restarts and must not be workload-writable.
type Config struct {
	StateDirectory  string   `json:"stateDirectory"`
	Executables     []string `json:"executables"`
	WorkDirectories []string `json:"workDirectories"`
	LogBytes        int64    `json:"logBytes"`
	// CgroupDirectory is an operator-delegated cgroup v2 subtree. Processes
	// must not be allowed to migrate out of it or modify its control files.
	CgroupDirectory string `json:"cgroupDirectory"`
	// WorkloadUID/GID must identify an unprivileged account distinct from the
	// supervisor. It must not have write access to state, sockets or cgroups.
	WorkloadUID uint32 `json:"workloadUID"`
	WorkloadGID uint32 `json:"workloadGID"`
}

// Service is a single-container exclusive process supervisor. It deliberately
// refuses to adopt processes after its own restart: only the same live supervisor
// can prove process ownership. Persisted uncertain executions block all new work.
type Service struct {
	mu       sync.Mutex
	config   Config
	lock     *os.File
	records  map[string]raw.Record
	active   string
	process  *exec.Cmd
	done     chan struct{}
	stopping bool
	closed   bool
	group    string
}

// Open recovers persisted observations without executing or signalling anything.
func Open(config Config) (*Service, error) {
	if config.WorkloadUID == 0 || config.WorkloadGID == 0 || config.WorkloadUID == uint32(os.Geteuid()) {
		return nil, errors.New("a distinct unprivileged workload UID and GID are required")
	}
	if !filepath.IsAbs(config.StateDirectory) || filepath.Clean(config.StateDirectory) != config.StateDirectory || config.LogBytes <= 0 || len(config.Executables) == 0 || len(config.WorkDirectories) == 0 {
		return nil, errors.New("canonical absolute state directory, authorized paths and positive log bound are required")
	}
	if !filepath.IsAbs(config.CgroupDirectory) || filepath.Clean(config.CgroupDirectory) != config.CgroupDirectory {
		return nil, errors.New("an operator-delegated cgroup v2 directory is required")
	}
	for _, name := range []string{"cgroup.controllers", "cgroup.events", "cgroup.kill"} {
		if _, err := os.Stat(filepath.Join(config.CgroupDirectory, name)); err != nil {
			return nil, fmt.Errorf("validate cgroup v2 delegation: %w", err)
		}
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(config.CgroupDirectory, &fs); err != nil {
		return nil, err
	}
	if fs.Type != unix.CGROUP2_SUPER_MAGIC {
		return nil, errors.New("cgroup directory must be on a cgroup v2 filesystem")
	}
	resolvedGroup, err := filepath.EvalSymlinks(config.CgroupDirectory)
	if err != nil || resolvedGroup != config.CgroupDirectory {
		return nil, errors.New("cgroup directory must be canonical without symlink components")
	}
	for _, path := range append(append([]string(nil), config.Executables...), config.WorkDirectories...) {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || !filepath.IsAbs(path) || resolved != path {
			return nil, fmt.Errorf("authorized path must exist and be canonical: %q", path)
		}
		if path == config.StateDirectory || strings.HasPrefix(config.StateDirectory, path+string(os.PathSeparator)) {
			return nil, errors.New("supervisor state must be outside authorized workload paths")
		}
	}
	if err := os.MkdirAll(config.StateDirectory, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(config.StateDirectory)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("supervisor state must be a private directory")
	}
	resolvedState, err := filepath.EvalSymlinks(config.StateDirectory)
	if err != nil || resolvedState != config.StateDirectory {
		return nil, errors.New("supervisor state must not contain symlink components")
	}
	lock, err := os.OpenFile(filepath.Join(config.StateDirectory, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.Join(errors.New("another Raw supervisor owns this state directory"), err, lock.Close())
	}
	s := &Service{config: config, lock: lock, records: map[string]raw.Record{}}
	entries, err := os.ReadDir(config.StateDirectory)
	if err != nil {
		return nil, errors.Join(err, s.Close())
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(config.StateDirectory, entry.Name()))
		if err != nil {
			return nil, errors.Join(err, s.Close())
		}
		var r raw.Record
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, errors.Join(err, s.Close())
		}
		if r.Task.ObjectUID == "" || r.Task.Logical.Validate() != nil || entry.Name() != key(r.Task)+".json" || (r.Deleted && !r.Cleaned) {
			return nil, errors.Join(errors.New("invalid persisted Raw execution identity"), s.Close())
		}
		if !r.Cleaned {
			r.Phase = "Unknown"
			r.Message = "Supervisor restarted; process ownership and cleanup require operator recovery"
			s.active = key(r.Task)
		}
		s.records[key(r.Task)] = r
	}
	return s, nil
}

func key(t rt.TaskIdentity) string {
	sum := sha256.Sum256([]byte(t.Logical.Namespace + "\x00" + t.Logical.Name + "\x00" + t.ObjectUID))
	return hex.EncodeToString(sum[:])
}

func (s *Service) persist(r raw.Record) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.config.StateDirectory, ".intent-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }() // Rename consumes the temporary file on success.
	if _, err := f.Write(data); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, filepath.Join(s.config.StateDirectory, key(r.Task)+".json")); err != nil {
		return err
	}
	dir, err := os.Open(s.config.StateDirectory)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func (s *Service) authorized(spec raw.ProcessSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	allowed := func(path string, candidates []string) bool {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || resolved != path {
			return false
		}
		for _, candidate := range candidates {
			if path == candidate {
				return true
			}
		}
		return false
	}
	if !allowed(spec.Argv[0], s.config.Executables) || !allowed(spec.Directory, s.config.WorkDirectories) {
		return fault.New(fault.PermissionDenied, "Raw executable or working directory is not authorized on this node")
	}
	return nil
}

// Observe returns an immutable owned observation; it never infers exit from a PID.
func (s *Service) Observe(ctx context.Context, t rt.TaskIdentity) (raw.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.observe(ctx, t)
}

func (s *Service) observe(ctx context.Context, t rt.TaskIdentity) (raw.Record, error) {
	if err := ctx.Err(); err != nil {
		return raw.Record{}, err
	}
	if s.closed {
		return raw.Record{}, fault.New(fault.Unavailable, "Raw supervisor closed")
	}
	r, ok := s.records[key(t)]
	if !ok {
		return raw.Record{}, fault.New(fault.NotFound, "Raw execution absent")
	}
	// Do not hand callers slices from the service's authoritative state.
	r.Spec.Argv = append([]string(nil), r.Spec.Argv...)
	r.Spec.Env = append([]string(nil), r.Spec.Env...)
	return r, nil
}

// Ensure persists intent before spawning exactly one process for a Task UID.
// It inherits no environment from the supervisor or Agent. The caller is the
// trusted admission boundary; this service must not be exposed to workloads.
func (s *Service) Ensure(ctx context.Context, t rt.TaskIdentity, spec raw.ProcessSpec) (raw.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return raw.Record{}, err
	}
	if s.closed {
		return raw.Record{}, fault.New(fault.Unavailable, "Raw supervisor closed")
	}
	if t.ObjectUID == "" || t.Logical.Validate() != nil {
		return raw.Record{}, fault.New(fault.InvalidArgument, "Raw Task identity required")
	}
	if err := s.authorized(spec); err != nil {
		return raw.Record{}, err
	}
	k := key(t)
	if r, ok := s.records[k]; ok {
		if !reflect.DeepEqual(r.Spec, spec) || r.Deleted {
			return raw.Record{}, fault.New(fault.Conflict, "Raw execution is immutable")
		}
		return s.observe(ctx, t)
	}
	if s.active != "" {
		return raw.Record{}, fault.New(fault.Conflict, "Raw container is occupied or cleanup is uncertain")
	}
	data, err := json.Marshal(spec)
	if err != nil {
		return raw.Record{}, err
	}
	var ownedSpec raw.ProcessSpec
	if err := json.Unmarshal(data, &ownedSpec); err != nil {
		return raw.Record{}, err
	}
	r := raw.Record{Task: t, Spec: ownedSpec, Phase: "Pending"}
	// Retain an uncertain in-memory record even if persistence fails after rename.
	s.records[k], s.active = r, k
	if err := s.persist(r); err != nil {
		return raw.Record{}, err
	}
	log, err := os.OpenFile(filepath.Join(s.config.StateDirectory, k+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return raw.Record{}, err
	}
	output := &boundedLog{file: log, remaining: s.config.LogBytes}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Env, cmd.Dir = append([]string{}, spec.Env...), spec.Directory
	cmd.Stdout, cmd.Stderr = output, output
	group := filepath.Join(s.config.CgroupDirectory, k)
	if err := os.Mkdir(group, 0700); err != nil {
		return raw.Record{}, errors.Join(err, log.Close())
	}
	groupFD, err := os.Open(group)
	if err != nil {
		return raw.Record{}, errors.Join(err, log.Close())
	}
	// CLONE_INTO_CGROUP places the process atomically, before any user code.
	// A kernel or delegation lacking this feature fails before execution.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, UseCgroupFD: true, CgroupFD: int(groupFD.Fd()), Credential: &syscall.Credential{Uid: s.config.WorkloadUID, Gid: s.config.WorkloadGID, Groups: []uint32{}}}
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		r.Phase, r.Message, r.Cleaned = "Failed", "Raw executable failed to start", true
		persistErr := s.persist(r)
		if persistErr == nil {
			s.active = ""
		} else {
			r.Phase, r.Message = "Unknown", "Raw start failure could not be persisted"
		}
		s.records[k] = r
		return r, errors.Join(err, groupFD.Close(), log.Close(), persistErr)
	}
	groupCloseErr := groupFD.Close()
	s.process, s.done, s.stopping = cmd, make(chan struct{}), false
	s.group = group
	r.Phase = "Running"
	s.records[k] = r
	// Always reap after Start, including a failed post-start status write.
	go s.reap(k, cmd, log)
	if err := errors.Join(groupCloseErr, s.persist(r)); err != nil {
		return raw.Record{}, err
	}
	return s.observe(ctx, t)
}

func (s *Service) reap(k string, cmd *exec.Cmd, log *os.File) {
	err := cmd.Wait()
	closeErr := log.Close()
	// Kill surviving workers, including processes that created another session.
	cleanupErr := os.WriteFile(filepath.Join(s.group, "cgroup.kill"), []byte("1"), 0600)
	cleaned := false
	if cleanupErr == nil {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			data, readErr := os.ReadFile(filepath.Join(s.group, "cgroup.events"))
			if readErr != nil {
				break
			}
			if strings.Contains("\n"+string(data), "\npopulated 0\n") {
				cleaned = true
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.records[k]
	r.Phase = "Succeeded"
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		r.Phase, r.Message = "Failed", "Raw process exited unsuccessfully"
	}
	if s.stopping {
		r.Phase, r.Message = "Stopped", "Raw process stop requested"
	}
	r.Cleaned = cleaned
	if !cleaned {
		r.Phase = "Unknown"
		r.Message = "Raw process exited but cgroup cleanup is unconfirmed"
	}
	if closeErr != nil {
		r.Message = "Raw log could not be closed"
	}
	persistErr := s.persist(r)
	if persistErr != nil {
		r.Phase, r.Message = "Unknown", "Raw result could not be persisted"
	}
	s.records[k], s.process = r, nil
	if cleaned && persistErr == nil {
		s.active = ""
	}
	close(s.done)
}

// Stop kills the execution's cgroup only while this supervisor still owns it.
func (s *Service) Stop(ctx context.Context, t rt.TaskIdentity) (raw.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.observe(ctx, t)
	if err != nil {
		return raw.Record{}, err
	}
	if r.Cleaned {
		r.Deleted = true
		if err := s.persist(r); err != nil {
			return raw.Record{}, err
		}
		s.records[key(t)] = r
		if s.active == key(t) {
			s.active = ""
		}
		return r, nil
	}
	if s.active == key(t) && s.process != nil {
		s.stopping = true
		if err := os.WriteFile(filepath.Join(s.group, "cgroup.kill"), []byte("1"), 0600); err != nil {
			return raw.Record{}, err
		}
	}
	return r, nil
}

// Close releases the supervisor lock only if no process is still running.
// Persisted uncertain executions remain reserved when the service is reopened.
func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.process != nil {
		return errors.New("stop and reap the active Raw process before closing supervision")
	}
	if s.closed {
		return nil
	}
	s.closed = true
	return s.lock.Close()
}

type boundedLog struct {
	mu        sync.Mutex
	file      *os.File
	remaining int64
}

func (w *boundedLog) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(data)
	if int64(len(data)) > w.remaining {
		data = data[:w.remaining]
	}
	if len(data) > 0 {
		written, err := w.file.Write(data)
		w.remaining -= int64(written)
		if err != nil {
			return written, err
		}
	}
	return n, nil // Drain excess output so bounded logs never block the workload.
}
