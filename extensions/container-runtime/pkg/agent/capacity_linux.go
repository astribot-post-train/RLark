//go:build linux

package agent

import (
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// ReadCgroupCapacity reports visible cgroup-v2 upper bounds, intersecting CPU
// quota with this process's affinity. Hidden ancestor constraints and concurrent
// usage are not measurable capacity or task-level resource enforcement. Unlimited
// memory is omitted rather than replaced with the host's RAM.
func ReadCgroupCapacity(mount string) (map[string]string, error) {
	if mount == "" {
		mount = "/sys/fs/cgroup"
	}
	if !filepath.IsAbs(mount) || filepath.Clean(mount) != mount {
		return nil, errors.New("canonical cgroup v2 mount required")
	}
	resolved, err := filepath.EvalSymlinks(mount)
	if err != nil || resolved != mount {
		return nil, errors.New("cgroup mount must exist without symlinks")
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(mount, &fs); err != nil {
		return nil, fmt.Errorf("stat cgroup mount: %w", err)
	}
	if fs.Type != unix.CGROUP2_SUPER_MAGIC {
		return nil, errors.New("cgroup v2 filesystem required")
	}
	data, err := readBounded("/proc/self/cgroup", 4096)
	if err != nil {
		return nil, fmt.Errorf("read current cgroup: %w", err)
	}
	current, err := cgroupPath(mount, string(data))
	if err != nil {
		return nil, err
	}
	var affinity unix.CPUSet
	if err := unix.SchedGetaffinity(0, &affinity); err != nil {
		return nil, fmt.Errorf("read CPU affinity: %w", err)
	}
	return readCgroupCapacity(mount, current, affinity.Count())
}

func cgroupPath(mount, data string) (string, error) {
	var path string
	for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			if path != "" {
				return "", errors.New("ambiguous current cgroup")
			}
			path = strings.TrimPrefix(line, "0::")
		}
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("canonical current cgroup v2 path required")
	}
	return filepath.Join(mount, strings.TrimPrefix(path, "/")), nil
}

func readCgroupCapacity(mount, current string, affinity int) (map[string]string, error) {
	relative, err := filepath.Rel(mount, current)
	if err != nil || relative == ".." || strings.HasPrefix(relative, "../") || affinity <= 0 {
		return nil, errors.New("current cgroup or affinity is invalid")
	}
	cpu := big.NewRat(int64(affinity), 1)
	var memory *big.Int
	for directory, depth := current, 0; ; directory, depth = filepath.Dir(directory), depth+1 {
		if depth > 256 {
			return nil, errors.New("cgroup hierarchy exceeds limit")
		}
		// The hierarchy root has no cpu.max or memory.max. In a cgroup namespace
		// it can expose both files; read them when present, never assume host RAM.
		for _, name := range []string{"cpu.max", "memory.max"} {
			data, err := readBounded(filepath.Join(directory, name), 128)
			if directory == mount && errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("read cgroup %s: %w", name, err)
			}
			if name == "cpu.max" {
				quota, err := parseCPUQuota(string(data))
				if err != nil {
					return nil, err
				}
				if quota != nil && quota.Cmp(cpu) < 0 {
					cpu = quota
				}
			} else {
				value := strings.TrimSpace(string(data))
				if value == "max" {
					continue
				}
				bytes, ok := new(big.Int).SetString(value, 10)
				if !ok || bytes.Sign() < 0 || !bytes.IsInt64() {
					return nil, errors.New("invalid memory.max")
				}
				if memory == nil || bytes.Cmp(memory) < 0 {
					memory = bytes
				}
			}
		}
		if directory == mount {
			break
		}
	}
	milli := new(big.Int).Quo(new(big.Int).Mul(cpu.Num(), big.NewInt(1000)), cpu.Denom())
	if !milli.IsInt64() {
		return nil, errors.New("CPU capacity is out of range")
	}
	result := map[string]string{"cpu": milli.String() + "m"}
	if memory != nil {
		result["memory"] = memory.String()
	}
	return result, nil
}

func parseCPUQuota(data string) (*big.Rat, error) {
	fields := strings.Fields(data)
	if len(fields) != 2 {
		return nil, errors.New("invalid cpu.max")
	}
	period, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || period <= 0 {
		return nil, errors.New("invalid cpu.max period")
	}
	if fields[0] == "max" {
		return nil, nil
	}
	quota, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || quota <= 0 {
		return nil, errors.New("invalid cpu.max quota")
	}
	return big.NewRat(quota, period), nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("observation file exceeds limit")
	}
	return data, nil
}
