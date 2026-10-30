// Package artifact verifies immutable Agent binaries before publication or use.
package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
)

// MaxBytes bounds one published binary and one authenticated download.
const MaxBytes int64 = 256 << 20

var (
	// ErrInvalid rejects unsupported metadata, paths and executable formats.
	ErrInvalid = errors.New("invalid Agent artifact")
	// ErrDigest means bytes disagree with their immutable release digest.
	ErrDigest     = errors.New("agent artifact digest mismatch")
	digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// Store reads verified bytes from a trusted publication source. It must honor
// cancellation and never return bytes outside the requested immutable digest.
type Store interface {
	Load(context.Context, ext.AgentReleaseSpec) ([]byte, error)
}

// ValidateSpec checks the supported release and canonical relative store path.
func ValidateSpec(spec ext.AgentReleaseSpec) error {
	if strings.TrimSpace(spec.Version) == "" || len(spec.Version) > 128 || strings.ContainsAny(spec.Version, "\x00\r\n") || spec.OS != "linux" || spec.Architecture != "amd64" && spec.Architecture != "arm64" || spec.Protocol != "v1" || !digestPattern.MatchString(spec.SHA256) || !fs.ValidPath(spec.ArtifactPath) || spec.ArtifactPath == "." || strings.ContainsAny(spec.ArtifactPath, "\\\x00\r\n") {
		return ErrInvalid
	}
	return nil
}

// Verify checks the exact digest, download bound and Linux executable platform.
func Verify(spec ext.AgentReleaseSpec, data []byte) error {
	if err := ValidateSpec(spec); err != nil {
		return err
	}
	if len(data) == 0 || int64(len(data)) > MaxBytes {
		return fmt.Errorf("%w: artifact size outside supported bound", ErrInvalid)
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != spec.SHA256 {
		return ErrDigest
	}
	f, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("%w: Linux ELF executable required", ErrInvalid)
	}
	defer func() { _ = f.Close() }() // bytes.Reader owns no external resource.
	if f.Class != elf.ELFCLASS64 || f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN || f.OSABI != elf.ELFOSABI_NONE && f.OSABI != elf.ELFOSABI_LINUX || spec.Architecture == "amd64" && f.Machine != elf.EM_X86_64 || spec.Architecture == "arm64" && f.Machine != elf.EM_AARCH64 {
		return fmt.Errorf("%w: executable platform mismatch", ErrInvalid)
	}
	return nil
}

// DirectoryStore reads only an operator-managed artifact root. The deployment
// must mount it read-only; it is never an upload or arbitrary file-read service.
type DirectoryStore struct{ directory string }

// NewDirectoryStore validates its root without starting background work.
func NewDirectoryStore(directory string) (*DirectoryStore, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || directory == "/" {
		return nil, fmt.Errorf("%w: canonical absolute artifact directory required", ErrInvalid)
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, fmt.Errorf("resolve artifact directory: %w", err)
	}
	if resolved != directory {
		return nil, fmt.Errorf("%w: artifact root must not contain symlinks", ErrInvalid)
	}
	info, err := os.Stat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect artifact directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: artifact root must be a directory", ErrInvalid)
	}
	return &DirectoryStore{directory: directory}, nil
}

// Load reads through an OS-confined root, rejects symlink components, then
// checks the digest and platform. No data is published before verification.
func (s *DirectoryStore) Load(ctx context.Context, spec ext.AgentReleaseSpec) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ValidateSpec(spec); err != nil {
		return nil, err
	}
	if s == nil || s.directory == "" {
		return nil, fmt.Errorf("%w: artifact store is not configured", ErrInvalid)
	}
	root, err := os.OpenRoot(s.directory)
	if err != nil {
		return nil, fmt.Errorf("open artifact root: %w", err)
	}
	defer func() { _ = root.Close() }() // Read-only directory handle.
	parts := strings.Split(spec.ArtifactPath, "/")
	for i := range parts {
		info, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil {
			return nil, fmt.Errorf("inspect artifact: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || i < len(parts)-1 && !info.IsDir() || i == len(parts)-1 && !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%w: artifact path must contain only directories and a regular file", ErrInvalid)
		}
	}
	f, err := root.Open(spec.ArtifactPath)
	if err != nil {
		return nil, fmt.Errorf("open artifact: %w", err)
	}
	defer func() { _ = f.Close() }() // Read errors remain authoritative.
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened artifact: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxBytes {
		return nil, fmt.Errorf("%w: bounded regular artifact required", ErrInvalid)
	}
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, reader: f}, MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read artifact: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := Verify(spec, data); err != nil {
		return nil, err
	}
	return data, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
