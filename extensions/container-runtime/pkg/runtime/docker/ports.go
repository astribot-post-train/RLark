package docker

import (
	"context"
	"fmt"
	"net"
)

const hostPortsLabel = "containers.rlark.io/host-tcp-ports"

// ValidateHostPortPlan validates an immutable host-network port list without
// making any network calls. Port policy is deliberately bounded here so a
// malformed execution plan cannot create an unbounded preflight workload.
func ValidateHostPortPlan(ports []int32) error {
	if len(ports) > 512 {
		return fmt.Errorf("docker port plan is too large")
	}
	seen := make(map[int32]struct{}, len(ports))
	for _, port := range ports {
		if port < 1024 || port > 65535 {
			return fmt.Errorf("docker host port %d is outside the permitted range", port)
		}
		if _, ok := seen[port]; ok {
			return fmt.Errorf("docker host port %d is duplicated", port)
		}
		seen[port] = struct{}{}
	}
	return nil
}

// CheckHostPorts is a best-effort preflight. The sockets are closed before it
// returns, so this is not a kernel reservation against foreign processes.
// RLark's whole-host allocation remains the durable ownership boundary.
func CheckHostPorts(ctx context.Context, ports []int32) error {
	if err := ValidateHostPortPlan(ports); err != nil {
		return err
	}
	for _, network := range []string{"tcp4", "tcp6"} {
		listeners := make([]net.Listener, 0, len(ports))
		for _, port := range ports {
			if err := ctx.Err(); err != nil {
				for _, listener := range listeners {
					_ = listener.Close()
				}
				return err
			}
			address := fmt.Sprintf("0.0.0.0:%d", port)
			if network == "tcp6" {
				address = fmt.Sprintf("[::]:%d", port)
			}
			listener, err := net.Listen(network, address)
			if err != nil {
				for _, open := range listeners {
					_ = open.Close()
				}
				return fmt.Errorf("docker host port %d is unavailable on %s: %w", port, network, err)
			}
			listeners = append(listeners, listener)
		}
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}
	return nil
}
