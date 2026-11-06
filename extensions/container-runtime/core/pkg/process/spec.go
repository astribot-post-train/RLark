// Package process defines the Raw workload wire contract without OS execution.
package process

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

const ProcessWorkload = "raw.rlark.io/process"

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ProcessSpec is immutable once an execution has been accepted. Env is an
// explicit allowlist: credentials and the Agent environment must not be inherited.
type ProcessSpec struct {
	Argv      []string `json:"argv"`
	Env       []string `json:"env"`
	Directory string   `json:"directory"`
}

// Validate rejects ambiguous executables and environment entries before effects.
// Empty Env is an explicit empty environment, never os.Environ(). The Supervisor
// must separately authorize directories and executable paths on its local node.
func (s ProcessSpec) Validate() error {
	if len(s.Argv) == 0 || !filepath.IsAbs(s.Argv[0]) || filepath.Clean(s.Argv[0]) != s.Argv[0] {
		return fmt.Errorf("a canonical absolute executable path is required")
	}
	if !filepath.IsAbs(s.Directory) || filepath.Clean(s.Directory) != s.Directory {
		return fmt.Errorf("a canonical absolute working directory is required")
	}
	for _, arg := range s.Argv {
		if strings.ContainsRune(arg, 0) {
			return fmt.Errorf("NUL in process argument")
		}
	}
	seen := map[string]bool{}
	for _, entry := range s.Env {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || !environmentName.MatchString(name) || seen[name] || strings.ContainsRune(entry, 0) {
			return fmt.Errorf("invalid or duplicate process environment entry")
		}
		seen[name] = true
	}
	return nil
}
