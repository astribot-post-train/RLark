//go:build linux

package docker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	rt "github.com/rlinf/rlark/apps/rlark/pkg/agent/execution/runtime"
	"golang.org/x/sys/unix"
)

// Journal retains cancellation and uncertain Docker creates across Agent
// restarts. One live owner holds its filesystem lock. Operators must preserve
// this private directory; removing records is not a supported recovery action.
type Journal struct {
	root           *os.Root
	lock           *os.File
	directory      string
	identity       os.FileInfo
	lockIdentity   os.FileInfo
	indexIdentity  os.FileInfo
	known          map[string]bool
	persistenceErr error
	gate           chan struct{}
	closed         bool
}

// executionRecord is local durable state, never API status. Creating without a
// container ID may mean a request is still committing in the daemon. Absence
// then cannot prove cleanup. Cancelled is permanent for this Task UID.
type executionRecord struct {
	Task        rt.TaskIdentity `json:"task"`
	Digest      string          `json:"digest,omitempty"`
	Creating    bool            `json:"creating,omitempty"`
	ContainerID string          `json:"containerID,omitempty"`
	Cancelled   bool            `json:"cancelled,omitempty"`
	Cleaned     bool            `json:"cleaned,omitempty"`
}

// OpenJournal opens an already provisioned canonical private directory. It
// performs no Docker calls and refuses a second owner or changed permissions.
func OpenJournal(directory string) (*Journal, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, errors.New("canonical absolute Docker state directory required")
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, fmt.Errorf("resolve Docker state directory: %w", err)
	}
	if resolved != directory {
		return nil, errors.New("docker state directory must not contain symlinks")
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, err
	}
	if !privateOwned(info, true) {
		return nil, errors.New("docker state directory must be private and owned by the Agent user")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	f, err := root.OpenFile(".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	fi, err := f.Stat()
	if err != nil || !privateOwned(fi, false) {
		return nil, errors.Join(errors.New("invalid Docker journal lock"), err, f.Close(), root.Close())
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.Join(fmt.Errorf("docker state already owned: %w", err), f.Close(), root.Close())
	}
	j := &Journal{root: root, lock: f, directory: directory, identity: info, lockIdentity: fi, gate: make(chan struct{}, 1)}
	j.gate <- struct{}{}
	if err := j.loadIndex(); err != nil {
		return nil, errors.Join(err, j.Close())
	}
	// Validate every retained record before permitting any runtime operation.
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, errors.Join(err, j.Close())
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		rec, err := j.readFile(entry.Name())
		if err != nil || rec == nil || entry.Name() != journalKey(rec.Task)+".json" || !j.known[strings.TrimSuffix(entry.Name(), ".json")] {
			return nil, errors.Join(errors.New("invalid retained Docker execution"), err, j.Close())
		}
	}
	for key := range j.known {
		if r, err := j.readFile(key + ".json"); err != nil || r == nil {
			return nil, errors.Join(errors.New("indexed Docker execution record missing"), err, j.Close())
		}
	}
	return j, nil
}

func privateOwned(info os.FileInfo, directory bool) bool {
	if info == nil || info.Mode().Perm()&0077 != 0 || info.IsDir() != directory || !directory && !info.Mode().IsRegular() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && (directory || stat.Nlink == 1)
}

func journalKey(task rt.TaskIdentity) string {
	sum := sha256.Sum256([]byte(task.Logical.Namespace + "\x00" + task.Logical.Name + "\x00" + task.ObjectUID))
	return hex.EncodeToString(sum[:])
}

func (j *Journal) acquire(ctx context.Context) (func(), error) {
	if j == nil {
		return nil, errors.New("docker execution journal required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-j.gate:
	}
	release := func() { j.gate <- struct{}{} }
	if j.closed || j.persistenceErr != nil {
		release()
		return nil, errors.New("docker execution journal closed")
	}
	current, err := os.Lstat(j.directory)
	if err != nil || !privateOwned(current, true) || !os.SameFile(current, j.identity) {
		release()
		return nil, errors.Join(errors.New("docker state directory lost or replaced"), err)
	}
	lockInfo, err := j.root.Lstat(".lock")
	if err != nil || !privateOwned(lockInfo, false) || !os.SameFile(lockInfo, j.lockIdentity) {
		release()
		return nil, errors.Join(errors.New("docker owner lock lost or replaced"), err)
	}
	indexInfo, err := j.root.Lstat(".index")
	if err != nil || !privateOwned(indexInfo, false) || !os.SameFile(indexInfo, j.indexIdentity) {
		release()
		return nil, errors.Join(errors.New("docker record index lost or replaced"), err)
	}
	return release, nil
}

// Close releases the exclusive owner only after active operations finish.
func (j *Journal) Close() error {
	if j == nil {
		return nil
	}
	<-j.gate
	defer func() { j.gate <- struct{}{} }()
	if j.closed {
		return nil
	}
	j.closed = true
	return errors.Join(j.lock.Close(), j.root.Close())
}

func (j *Journal) read(task rt.TaskIdentity) (*executionRecord, error) {
	key := journalKey(task)
	r, err := j.readFile(key + ".json")
	if err == nil && r == nil && j.known[key] {
		return nil, errors.New("indexed Docker execution record missing")
	}
	if err != nil || r == nil {
		return r, err
	}
	if r.Task.ObjectUID != task.ObjectUID || r.Task.Logical != task.Logical {
		return nil, errors.New("docker execution journal identity conflict")
	}
	return r, nil
}
func (j *Journal) readFile(name string) (*executionRecord, error) {
	f, err := j.root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // Read-only close has no persistence responsibility.
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !privateOwned(info, false) || info.Size() > 16384 {
		return nil, errors.New("invalid Docker execution record file")
	}
	d := json.NewDecoder(io.LimitReader(f, 16385))
	d.DisallowUnknownFields()
	var r executionRecord
	if err := d.Decode(&r); err != nil {
		return nil, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, errors.New("exactly one Docker execution record required")
	}
	if validateTask(r.Task) != nil || r.Task.Generation < 1 || r.Cleaned && !r.Cancelled || r.Digest != "" && len(r.Digest) != 64 || r.Creating && r.Digest == "" || r.ContainerID != "" && !r.Creating {
		return nil, errors.New("invalid Docker execution record")
	}
	return &r, nil
}
func (j *Journal) write(r executionRecord) error {
	if err := j.register(journalKey(r.Task)); err != nil {
		j.persistenceErr = err
		return err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	name := ".pending-" + hex.EncodeToString(random[:])
	f, err := j.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = j.root.Remove(name) }() // Rename consumes this temporary file on success.
	if _, err := f.Write(data); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := j.root.Rename(name, journalKey(r.Task)+".json"); err != nil {
		return err
	}
	dir, err := j.root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

// loadIndex retains a durable inventory so a deleted individual cancellation
// record is an error rather than mistaken for a never-started Task.
func (j *Journal) loadIndex() error {
	j.known = map[string]bool{}
	f, err := j.root.OpenFile(".index", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		entries, readErr := fs.ReadDir(j.root.FS(), ".")
		if readErr != nil {
			return readErr
		}
		for _, e := range entries {
			if filepath.Ext(e.Name()) == ".json" {
				return errors.New("docker journal index missing")
			}
		}
		f, err = j.root.OpenFile(".index", os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return err
		}
		if err := errors.Join(f.Sync(), f.Close()); err != nil {
			return err
		}
		dir, dirErr := j.root.Open(".")
		if dirErr != nil {
			return dirErr
		}
		if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
			return err
		}
		f, err = j.root.OpenFile(".index", os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }() // Read-only close.
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !privateOwned(info, false) || info.Size() > 16<<20 {
		return errors.New("invalid Docker record index")
	}
	data, err := io.ReadAll(io.LimitReader(f, (16<<20)+1))
	if err != nil {
		return err
	}
	if len(data)%65 != 0 {
		return errors.New("incomplete Docker record index")
	}
	for len(data) > 0 {
		key := string(data[:64])
		if data[64] != '\n' {
			return errors.New("invalid Docker record index entry")
		}
		if decoded, err := hex.DecodeString(key); err != nil || len(decoded) != 32 || strings.ToLower(key) != key {
			return errors.New("invalid Docker record index key")
		}
		j.known[key] = true
		data = data[65:]
	}
	j.indexIdentity = info
	return nil
}
func (j *Journal) register(key string) error {
	if j.known[key] {
		return nil
	}
	if len(j.known) >= (16<<20)/65 {
		return errors.New("docker journal index full; operator recovery required")
	}
	f, err := j.root.OpenFile(".index", os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(key + "\n"); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	j.known[key] = true
	return nil
}

// Check verifies the live directory, process lock and record-index ownership
// without contacting Docker or changing a Task record.
func (j *Journal) Check(ctx context.Context) error {
	unlock, err := j.acquire(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return nil
}
