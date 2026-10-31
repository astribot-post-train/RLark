package docker

import (
	"errors"
	"path/filepath"
	"strings"
)

const DefaultLocalHost = "unix:///var/run/docker.sock"

// ValidateLocalHost limits the node Agent to a canonical host Unix socket.
// Remote Engine endpoints need a separate transport/authentication contract.
func ValidateLocalHost(host string) error {
	path, ok := strings.CutPrefix(host, "unix://")
	if !ok || path == "/" || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n%?#") {
		return errors.New("docker host must be a canonical absolute unix:/// socket path")
	}
	return nil
}
