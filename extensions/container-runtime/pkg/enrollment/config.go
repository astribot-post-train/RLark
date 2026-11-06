package enrollment

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/util/validation"
)

// LoadProfile reads one private controller-local deployment profile.
func LoadProfile(path string) (Profile, error) {
	var p Profile
	info, err := os.Lstat(path)
	if err != nil {
		return p, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return p, errors.New("enrollment profile must be a private regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return p, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return p, err
	}
	if d.Decode(new(any)) != io.EOF {
		return p, errors.New("one strict profile required")
	}
	return p, p.Validate()
}

// Validate checks server configuration before any controller or remote effect.
func (p Profile) Validate() error {
	if p.ClusterUID == "" || p.Environment == "" || p.NetworkScope == "" || p.Endpoint == "" || len(p.Executables) == 0 || len(p.WorkDirectories) == 0 || len(p.GatewayArgv) == 0 {
		return errors.New("exact cluster, environment, endpoint, workload paths and gateway launch configuration required")
	}
	for _, path := range []string{p.AgentCA, p.AgentCAKey, p.ServerCA, p.SupervisorBinary, p.GatewayBinary, p.InstallDirectory, p.StateDirectory, p.Python, p.SSH.IdentityFile, p.SSH.KnownHosts, p.SSH.WorkerIdentityFile, p.SSH.WorkerKnownHosts} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
			return errors.New("canonical absolute deployment paths required")
		}
	}
	if len(p.AllowedGatewayCIDRs) == 0 || len(p.AllowedWorkerCIDRs) == 0 {
		return errors.New("explicit SSH target CIDR policy required")
	}
	for _, cidr := range append(append([]string{}, p.AllowedGatewayCIDRs...), p.AllowedWorkerCIDRs...) {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return errors.New("invalid SSH target CIDR policy")
		}
	}
	for _, paths := range []map[string][]string{p.ExecutablesByNode, p.WorkDirectoriesByNode} {
		for name, values := range paths {
			if len(validation.IsDNS1123Subdomain(name)) != 0 || len(values) == 0 {
				return errors.New("per-node workload paths require a valid node name and nonempty allowlist")
			}
		}
	}
	return p.SSH.validateControlDirectory()
}

func (p Profile) workloadPaths(name string) ([]string, []string, error) {
	executables, directories := p.Executables, p.WorkDirectories
	if paths, ok := p.ExecutablesByNode[name]; ok {
		executables = paths
	}
	if paths, ok := p.WorkDirectoriesByNode[name]; ok {
		directories = paths
	}
	for _, paths := range [][]string{executables, directories} {
		if len(paths) == 0 {
			return nil, nil, errors.New("node workload allowlists must not be empty")
		}
		for _, path := range paths {
			if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
				return nil, nil, errors.New("canonical absolute node workload paths required")
			}
		}
	}
	return executables, directories, nil
}
