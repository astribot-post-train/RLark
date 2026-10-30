//go:build linux

package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/allocation/companion"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/artifact"
	"golang.org/x/sys/unix"
)

// UpdaterPaths isolates one installed Agent and its durable upgrade journal.
// Both directories must already exist, be private to the effective user, and
// contain no symlink components. The supervisor state must use another path.
type UpdaterPaths struct{ InstallPath, StateDirectory string }

func (p UpdaterPaths) Validate() error {
	for _, path := range []string{p.InstallPath, p.StateDirectory} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || strings.ContainsAny(path, "\x00\r\n") {
			return errors.New("canonical absolute updater paths required")
		}
	}
	parent := filepath.Dir(p.InstallPath)
	if parent == p.StateDirectory || strings.HasPrefix(p.StateDirectory, parent+"/") || strings.HasPrefix(parent, p.StateDirectory+"/") {
		return errors.New("install and updater state directories must be separate")
	}
	return nil
}

type updaterFiles struct {
	paths UpdaterPaths
	locks []*os.File
}

func privatePath(path string, directory bool) (os.FileInfo, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	if resolved != path {
		return nil, errors.New("updater paths must not contain symlinks")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0022 != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 || (directory && (!info.IsDir() || info.Mode().Perm()&0077 != 0)) || (!directory && (!info.Mode().IsRegular() || stat.Nlink != 1)) {
		return nil, errors.New("private owned updater directory or regular file required")
	}
	return info, nil
}

func openUpdaterFiles(paths UpdaterPaths) (*updaterFiles, error) {
	if err := paths.Validate(); err != nil {
		return nil, err
	}
	for _, dir := range []string{filepath.Dir(paths.InstallPath), paths.StateDirectory} {
		if _, err := privatePath(dir, true); err != nil {
			return nil, fmt.Errorf("validate updater directory: %w", err)
		}
	}
	info, err := privatePath(paths.InstallPath, false)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0100 == 0 {
		return nil, errors.New("installed Agent must be executable by its owner")
	}
	f := &updaterFiles{paths: paths}
	for _, path := range []string{paths.InstallPath + ".upgrade-lock", filepath.Join(paths.StateDirectory, "upgrade-lock")} {
		fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
		if err != nil {
			f.close()
			return nil, err
		}
		lock := os.NewFile(uintptr(fd), path)
		f.locks = append(f.locks, lock)
		if _, err = privatePath(path, false); err == nil {
			err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		}
		if err != nil {
			f.close()
			return nil, fmt.Errorf("lock Agent updater: %w", err)
		}
	}
	return f, nil
}
func (f *updaterFiles) close() {
	for _, lock := range f.locks {
		_ = lock.Close()
	} // Closing releases flock, including startup failures.
	f.locks = nil
}
func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }() // Sync errors remain authoritative.
	return dir.Sync()
}

// durableWrite never follows an existing target and makes the rename durable.
func durableWrite(path string, data []byte, mode os.FileMode) error {
	if _, err := os.Lstat(path); err == nil {
		if _, err := privatePath(path, false); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".agent-upgrade-*")
	if err != nil {
		return err
	}
	defer func() { _ = temp.Close(); _ = os.Remove(temp.Name()) }() // Best-effort removal of an uncommitted temporary file.
	if err := temp.Chmod(mode.Perm()); err != nil {
		return err
	}
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
func boundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }() // Read/stat failures remain authoritative.
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit {
		return nil, errors.New("bounded regular updater file required")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = errors.New("updater file exceeds limit")
	}
	return data, err
}
func binaryDigest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func fileDigest(path string) (string, error) {
	data, err := boundedFile(path, artifact.MaxBytes)
	if err != nil {
		return "", err
	}
	return binaryDigest(data), nil
}
func (f *updaterFiles) receiptPath() string {
	return filepath.Join(f.paths.StateDirectory, "upgrade.json")
}
func (f *updaterFiles) load() (*upgradeJournal, error) {
	if _, err := privatePath(f.receiptPath(), false); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	data, err := boundedFile(f.receiptPath(), upgradeBodyLimit)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var receipt upgradeJournal
	if err := decoder.Decode(&receipt); err != nil {
		return nil, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("one upgrade receipt required")
	}
	if err := receipt.validate(); err != nil {
		return nil, err
	}
	return &receipt, nil
}
func (f *updaterFiles) save(receipt *upgradeJournal) error {
	if err := receipt.validate(); err != nil {
		return err
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if len(data) > upgradeBodyLimit {
		return errors.New("upgrade receipt exceeds limit")
	}
	return durableWrite(f.receiptPath(), data, 0600)
}
func (f *updaterFiles) install(r *upgradeJournal, data []byte) error {
	info, err := privatePath(f.paths.InstallPath, false)
	if err != nil {
		return err
	}
	current, err := boundedFile(f.paths.InstallPath, artifact.MaxBytes)
	if err != nil {
		return err
	}
	backup := f.paths.InstallPath + ".backup"
	switch binaryDigest(current) {
	case r.Release.SHA256:
		if _, err := privatePath(backup, false); err != nil {
			return err
		}
		digest, err := fileDigest(backup)
		if err != nil {
			return err
		}
		if digest != r.SourceSHA256 {
			return ErrUpgradeIdentity
		}
		return nil
	case r.SourceSHA256:
	default:
		return ErrUpgradeIdentity
	}
	// Launchers verify only an operator-exported or updater-verified Agent digest.
	// Persist both candidates before rename so either crash boundary can restart.
	for suffix, digest := range map[string]string{".upgrade-source.sha256": r.SourceSHA256, ".upgrade-target.sha256": r.Release.SHA256} {
		metadata := filepath.Join(filepath.Dir(f.paths.InstallPath), filepath.Base(f.paths.InstallPath)+suffix)
		if err := durableWrite(metadata, []byte(digest+"  "+filepath.Base(f.paths.InstallPath)+"\n"), 0600); err != nil {
			return err
		}
	}
	if err := durableWrite(backup, current, info.Mode()); err != nil {
		return fmt.Errorf("persist Agent backup: %w", err)
	}
	// Staging is durable in private state even when install is on another device.
	staged := filepath.Join(f.paths.StateDirectory, "agent-staged")
	if err := durableWrite(staged, data, info.Mode()); err != nil {
		return err
	}
	return durableWrite(f.paths.InstallPath, data, info.Mode())
}
func (f *updaterFiles) rollback(r *upgradeJournal) error {
	backup := f.paths.InstallPath + ".backup"
	info, err := privatePath(backup, false)
	if err != nil {
		return err
	}
	data, err := boundedFile(backup, artifact.MaxBytes)
	if err != nil {
		return err
	}
	if binaryDigest(data) != r.SourceSHA256 {
		return ErrUpgradeIdentity
	}
	current, err := fileDigest(f.paths.InstallPath)
	if err != nil {
		return err
	}
	if current != r.SourceSHA256 && current != r.Release.SHA256 {
		return ErrUpgradeIdentity
	}
	return durableWrite(f.paths.InstallPath, data, info.Mode())
}

type upgradeJournal struct {
	Scope           string                    `json:"scope"`
	InstallPath     string                    `json:"installPath"`
	Operation       ext.NativeObjectReference `json:"operation"`
	Allocation      ext.NativeObjectReference `json:"allocation"`
	ResourceVersion string                    `json:"resourceVersion"`
	AttemptID       string                    `json:"attemptID"`
	SourceVersion   string                    `json:"sourceVersion"`
	SourceSHA256    string                    `json:"sourceSHA256"`
	Release         ReleaseManifest           `json:"release"`
	Phase           string                    `json:"phase"`
}

func (r *upgradeJournal) validate() error {
	if r == nil || r.Scope == "" || r.InstallPath == "" || r.Operation.Namespace == "" || r.Operation.Name == "" || r.Operation.UID == "" || r.Allocation.Namespace != r.Operation.Namespace || r.Allocation.Name == "" || r.Allocation.UID == "" || r.ResourceVersion == "" || !companion.ValidUpgradeAttempt(r.AttemptID) || strings.TrimSpace(r.SourceVersion) == "" || len(r.SourceVersion) > 128 || strings.ContainsAny(r.SourceVersion, "\x00\r\n") || r.Release.Namespace != r.Operation.Namespace || r.SourceVersion == r.Release.Version || r.SourceSHA256 == r.Release.SHA256 {
		return ErrUpgradeIdentity
	}
	for _, digest := range []string{r.SourceSHA256, r.Scope} {
		data, err := hex.DecodeString(digest)
		if err != nil || len(data) != sha256.Size || strings.ToLower(digest) != digest {
			return ErrUpgradeIdentity
		}
	}
	if _, err := manifestSpec(&r.Release); err != nil {
		return err
	}
	switch r.Phase {
	case "Reserved", "Switching", "Rollback", "Succeeded", "RolledBack", "Abandoned":
	default:
		return ErrUpgradeIdentity
	}
	return nil
}
