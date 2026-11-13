package main

import (
	"bytes"
	"fmt"
	"log"
	"net"
	"net/url"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"
)

// uplinkCheck refuses a machine that dials Station API from an address it
// does not have in the network of the gateway it dials through (a robot's
// computer that holds an address of the venue's network behind the robot's
// router): nothing has given it that address, and whatever is given it later
// takes the machine's answers. The machine is to dial from its own address,
// and the device before it to let it out (the site's leafUplink).
//
// A node that moves to another cluster was admitted before this rule and
// keeps its uplink through the move: it is refused only if something else
// answers for the address now. The probe is sent where the machine is, so it
// finds the device before it holding the address, not one beyond that.
func uplinkCheck(server string, moving bool) error {
	target, err := url.Parse(server)
	if err != nil {
		return err
	}
	// What the kernel chooses for a packet to the server; nothing is sent.
	conn, err := net.Dial("udp4", net.JoinHostPort(target.Hostname(), "9"))
	if err != nil {
		return fmt.Errorf("no route to Station API at %s: %w", target.Hostname(), err)
	}
	source, remote := conn.LocalAddr().(*net.UDPAddr).IP, conn.RemoteAddr().(*net.UDPAddr).IP
	conn.Close()
	route, err := exec.Command("/bin/sh", "-c", "PATH=/usr/sbin:/sbin:/usr/bin:/bin ip -4 -o route get "+remote.String()).Output()
	if err != nil {
		return fmt.Errorf("ip route get %s: %w", remote, err)
	}
	fields := strings.Fields(string(route))
	after := func(word string) string {
		if i := slices.Index(fields, word); i >= 0 && i+1 < len(fields) {
			return fields[i+1]
		}
		return ""
	}
	gateway := net.ParseIP(after("via"))
	if gateway == nil {
		return nil // the server is in a network this machine is in itself
	}
	device, err := net.InterfaceByName(after("dev"))
	if err != nil {
		return fmt.Errorf("the interface of the route to %s: %w", remote, err)
	}
	addresses, err := device.Addrs()
	if err != nil {
		return err
	}
	own := "no address"
	for _, address := range addresses {
		if network, ok := address.(*net.IPNet); ok && network.Contains(gateway) {
			if network.IP.Equal(source) {
				return nil
			}
			own = network.IP.String()
		}
	}
	if !moving {
		return fmt.Errorf("this machine dials out from %s, which is not an address in the network of its gateway %s (there it has %s): nothing has given it that address, and whatever is given it later takes this machine's answers. Remove the address and the routes that name it as their source, so that the machine dials from its own address; the device at %s lets it out once the first line of the add-node command has run there",
			source, gateway, own, gateway)
	}
	holder, err := answersFor(device, source)
	if err != nil {
		return fmt.Errorf("probe for another holder of %s on %s: %w", source, device.Name, err)
	}
	if holder != nil {
		return fmt.Errorf("this machine dials out from %s, and %s on %s answers for that address too: the machine's answers go there. The node stays in its cluster; take the address from one of the two first", source, holder, device.Name)
	}
	log.Printf("this node dials out from %s, not an address in the network of its gateway %s. Nothing else answers for it on %s now: that says the device at %s does not hold it, and nothing about the network beyond that device, which this node cannot probe", source, gateway, device.Name, gateway)
	return nil
}

// answersFor sends one ARP probe for address on device (RFC 5227: from no
// address, so that nobody's table changes) and returns the hardware address
// of what answers within two seconds, nil if nothing does.
func answersFor(device *net.Interface, address net.IP) (net.HardwareAddr, error) {
	const arp = syscall.ETH_P_ARP<<8&0xff00 | syscall.ETH_P_ARP>>8 // in network byte order
	socket, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_DGRAM, arp)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(socket)
	if err := syscall.Bind(socket, &syscall.SockaddrLinklayer{Protocol: arp, Ifindex: device.Index}); err != nil {
		return nil, err
	}
	if err := syscall.SetsockoptTimeval(socket, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &syscall.Timeval{Usec: 200000}); err != nil {
		return nil, err
	}
	// Ethernet, IPv4, request; sender (hardware, 0.0.0.0); target (unknown, address).
	probe := append([]byte{0, 1, 8, 0, 6, 4, 0, 1}, device.HardwareAddr...)
	probe = append(append(probe, make([]byte, 10)...), address.To4()...)
	everyone := syscall.SockaddrLinklayer{Protocol: arp, Ifindex: device.Index, Halen: 6, Addr: [8]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}}
	if err := syscall.Sendto(socket, probe, 0, &everyone); err != nil {
		return nil, err
	}
	packet := make([]byte, 64)
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		// Sender hardware address at 8, sender address at 14.
		if n, _, err := syscall.Recvfrom(socket, packet, 0); err == nil && n >= 28 && bytes.Equal(packet[14:18], address.To4()) && !bytes.Equal(packet[8:14], device.HardwareAddr) {
			return net.HardwareAddr(bytes.Clone(packet[8:14])), nil
		}
	}
	return nil, nil
}
