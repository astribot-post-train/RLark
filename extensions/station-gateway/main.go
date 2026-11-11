// station-gateway is a minimal WireGuard bootstrap reconciler. It manages only
// the explicitly named and labelled link and its protocol-tagged routes.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

const owner = "rlark-station-gateway/v1"
const routeProtocol = 242
const routePriority = 49321

type config struct {
	Interface    string        `json:"interface"`
	Address      string        `json:"address"`
	ListenPort   int           `json:"listenPort"`
	MTU          int           `json:"mtu"`
	Peers        []peer        `json:"peers"`
	Routes       []string      `json:"routes"`
	UplinkRoutes []uplinkRoute `json:"uplinkRoutes"`
}
type uplinkRoute struct {
	Destination string `json:"destination"`
	Gateway     string `json:"gateway"`
	Source      string `json:"source"`
	Interface   string `json:"interface"`
}
type peer struct {
	PublicKey        string   `json:"publicKey"`
	Endpoint         string   `json:"endpoint"`
	AllowedIPs       []string `json:"allowedIPs"`
	KeepaliveSeconds int      `json:"keepaliveSeconds"`
}
type desired struct {
	config
	address *netlink.Addr
	peers   []wgtypes.PeerConfig
	routes  []*net.IPNet
}

func parse(data []byte) (*desired, error) {
	var c config
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("configuration must contain exactly one JSON object")
	}
	if c.Interface == "" || len(c.Interface) > 15 || strings.ContainsAny(c.Interface, "/ \t\n") {
		return nil, errors.New("invalid interface name")
	}
	if c.ListenPort < 1 || c.ListenPort > 65535 || c.MTU < 576 || c.MTU > 9000 {
		return nil, errors.New("invalid listen port or MTU")
	}
	addr, err := netlink.ParseAddr(c.Address)
	if err != nil {
		return nil, fmt.Errorf("address: %w", err)
	}
	d := &desired{config: c, address: addr}
	seen := map[string]bool{}
	prefixes := map[string]bool{}
	for _, p := range c.Peers {
		key, e := wgtypes.ParseKey(p.PublicKey)
		if e != nil {
			return nil, fmt.Errorf("peer public key: %w", e)
		}
		if seen[key.String()] {
			return nil, errors.New("duplicate peer key")
		}
		seen[key.String()] = true
		if p.KeepaliveSeconds < 0 || p.KeepaliveSeconds > 3600 {
			return nil, errors.New("invalid keepalive")
		}
		pc := wgtypes.PeerConfig{PublicKey: key, ReplaceAllowedIPs: true}
		if p.Endpoint != "" {
			pc.Endpoint, e = net.ResolveUDPAddr("udp", p.Endpoint)
			if e != nil {
				return nil, fmt.Errorf("endpoint: %w", e)
			}
		}
		keep := time.Duration(p.KeepaliveSeconds) * time.Second
		pc.PersistentKeepaliveInterval = &keep
		for _, s := range p.AllowedIPs {
			_, n, e := net.ParseCIDR(s)
			if e != nil {
				return nil, fmt.Errorf("allowed IP: %w", e)
			}
			if prefixes[n.String()] {
				return nil, fmt.Errorf("duplicate allowed prefix %s", n)
			}
			prefixes[n.String()] = true
			pc.AllowedIPs = append(pc.AllowedIPs, *n)
		}
		if len(pc.AllowedIPs) == 0 {
			return nil, errors.New("peer requires allowed IPs")
		}
		d.peers = append(d.peers, pc)
	}
	for _, s := range c.Routes {
		_, n, e := net.ParseCIDR(s)
		if e != nil {
			return nil, fmt.Errorf("route: %w", e)
		}
		if n.String() == "0.0.0.0/0" || n.String() == "::/0" {
			return nil, errors.New("default routes are forbidden")
		}
		d.routes = append(d.routes, n)
	}
	for _, route := range c.UplinkRoutes {
		ip, dst, e := net.ParseCIDR(route.Destination)
		if e != nil || ip.To4() == nil || dst.String() != ip.String()+"/32" || net.ParseIP(route.Gateway).To4() == nil || net.ParseIP(route.Source).To4() == nil || route.Interface == "" || strings.ContainsAny(route.Interface, "/ \t\n") {
			return nil, errors.New("uplink route requires an IPv4 /32 destination, gateway, source and interface")
		}
	}
	return d, nil
}
func keyFile(path string) (wgtypes.Key, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return wgtypes.ParseKey(strings.TrimSpace(string(data)))
	}
	if !errors.Is(err, os.ErrNotExist) {
		return wgtypes.Key{}, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return wgtypes.Key{}, err
	}
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return key, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return keyFile(path)
	}
	if err != nil {
		return key, err
	}
	if _, err = f.WriteString(key.String() + "\n"); err != nil {
		f.Close()
		_ = os.Remove(path)
		return key, err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(path)
		return key, err
	}
	return key, f.Close()
}

func deviceConfig(d *desired, key wgtypes.Key, current *wgtypes.Device) (wgtypes.Config, bool) {
	port := d.ListenPort
	cfg := wgtypes.Config{PrivateKey: &key, ListenPort: &port, ReplacePeers: true, Peers: append([]wgtypes.PeerConfig(nil), d.peers...)}
	changed := current.PrivateKey != key || current.ListenPort != port || len(current.Peers) != len(cfg.Peers)
	existing := make(map[wgtypes.Key]wgtypes.Peer, len(current.Peers))
	for _, p := range current.Peers {
		existing[p.PublicKey] = p
	}
	for i := range cfg.Peers {
		want := &cfg.Peers[i]
		got, ok := existing[want.PublicKey]
		if !ok {
			changed = true
			continue
		}
		if want.Endpoint == nil {
			want.Endpoint = got.Endpoint
		} else if got.Endpoint == nil || want.Endpoint.String() != got.Endpoint.String() {
			changed = true
		}
		if want.PersistentKeepaliveInterval == nil || *want.PersistentKeepaliveInterval != got.PersistentKeepaliveInterval {
			changed = true
		}
		wantIPs, gotIPs := make([]string, 0, len(want.AllowedIPs)), make([]string, 0, len(got.AllowedIPs))
		for _, ip := range want.AllowedIPs {
			wantIPs = append(wantIPs, ip.String())
		}
		for _, ip := range got.AllowedIPs {
			gotIPs = append(gotIPs, ip.String())
		}
		sort.Strings(wantIPs)
		sort.Strings(gotIPs)
		if strings.Join(wantIPs, ",") != strings.Join(gotIPs, ",") {
			changed = true
		}
	}
	return cfg, changed
}

func apply(d *desired, key wgtypes.Key) error {
	link, err := netlink.LinkByName(d.Interface)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if !errors.As(err, &notFound) {
			return err
		}
		link = &netlink.GenericLink{LinkAttrs: netlink.LinkAttrs{Name: d.Interface, Alias: owner}, LinkType: "wireguard"}
		if err = netlink.LinkAdd(link); err != nil {
			return fmt.Errorf("create interface: %w", err)
		}
		link, err = netlink.LinkByName(d.Interface)
		if err != nil {
			return err
		}
		// IFLA_IFALIAS supplied during RTM_NEWLINK is ignored by some kernels.
		// Explicitly set it only after our successful exclusive creation.
		if err = netlink.LinkSetAlias(link, owner); err != nil {
			return fmt.Errorf("mark new interface: %w", err)
		}
		link, err = netlink.LinkByName(d.Interface)
		if err != nil {
			return err
		}
	}
	if link.Type() != "wireguard" || link.Attrs().Alias != owner {
		return fmt.Errorf("interface %s is not owned by station gateway", d.Interface)
	}
	// Never add a new address alongside an existing one: address migration
	// requires a separate interface and an independently verified path.
	addresses, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("inspect interface addresses: %w", err)
	}
	for _, address := range addresses {
		if address.IPNet == nil || address.IPNet.String() != d.address.IPNet.String() {
			return fmt.Errorf("interface %s has a different IPv4 address; use a new interface for migration", d.Interface)
		}
	}
	for _, r := range d.UplinkRoutes {
		link, err := netlink.LinkByName(r.Interface)
		if err != nil {
			return fmt.Errorf("uplink interface: %w", err)
		}
		_, dst, _ := net.ParseCIDR(r.Destination)
		gw, src := net.ParseIP(r.Gateway).To4(), net.ParseIP(r.Source).To4()
		addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			return fmt.Errorf("uplink addresses: %w", err)
		}
		foundSource := false
		for _, a := range addrs {
			if a.IP.Equal(src) {
				foundSource = true
			}
		}
		if !foundSource {
			return fmt.Errorf("uplink source %s is not assigned to %s", r.Source, r.Interface)
		}
		existing, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Dst: dst, Table: 254}, netlink.RT_FILTER_DST|netlink.RT_FILTER_TABLE)
		if err != nil {
			return fmt.Errorf("inspect uplink route: %w", err)
		}
		if len(existing) > 0 {
			if len(existing) != 1 || existing[0].LinkIndex != link.Attrs().Index || existing[0].Protocol != routeProtocol || existing[0].Priority != routePriority || !existing[0].Gw.Equal(gw) || !existing[0].Src.Equal(src) {
				return fmt.Errorf("uplink route %s conflicts with an existing route", dst)
			}
			continue
		}
		if err := netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Dst: dst, Gw: gw, Src: src, Protocol: routeProtocol, Priority: routePriority, Table: 254}); err != nil {
			return fmt.Errorf("add uplink route %s: %w", dst, err)
		}
	}
	// Preflight route removal before touching the device. Route protocol and
	// metric are not proof of ownership, so never delete any kernel route.
	existing, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{LinkIndex: link.Attrs().Index, Protocol: routeProtocol}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_PROTOCOL)
	if err != nil {
		return err
	}
	wanted := map[string]bool{}
	for _, dst := range d.routes {
		wanted[dst.String()] = true
	}
	for _, r := range existing {
		if r.Priority == routePriority && r.Dst != nil && !wanted[r.Dst.String()] {
			return fmt.Errorf("route %s requires manual removal before changing configuration", r.Dst)
		}
	}
	c, err := wgctrl.New()
	if err != nil {
		return err
	}
	defer c.Close()
	current, err := c.Device(d.Interface)
	if err != nil {
		return fmt.Errorf("observe wireguard: %w", err)
	}
	if config, changed := deviceConfig(d, key, current); changed {
		if err = c.ConfigureDevice(d.Interface, config); err != nil {
			return fmt.Errorf("configure wireguard: %w", err)
		}
	}
	if err = netlink.LinkSetMTU(link, d.MTU); err != nil {
		return err
	}
	if err = netlink.AddrReplace(link, d.address); err != nil {
		return err
	}
	if err = netlink.LinkSetUp(link); err != nil {
		return err
	}
	for _, dst := range d.routes {
		found := false
		for _, r := range existing {
			if r.Dst != nil && r.Dst.String() == dst.String() && r.Priority == routePriority && r.Gw == nil && r.Table == 254 && r.Type == 1 {
				found = true
			}
		}
		if !found {
			r := netlink.Route{LinkIndex: link.Attrs().Index, Dst: dst, Protocol: routeProtocol, Priority: routePriority, Table: 254, Type: 1}
			if err = netlink.RouteAdd(&r); err != nil {
				return fmt.Errorf("add route %s: %w", dst, err)
			}
		}
	}
	return nil
}
func main() {
	defaultConfigPath := os.Getenv("STATION_GATEWAY_CONFIG")
	if defaultConfigPath == "" {
		defaultConfigPath = "/config/config.json"
	}
	configPath := flag.String("config", defaultConfigPath, "desired configuration")
	keyPath := flag.String("key", "/var/lib/station/wg/private.key", "node-local private key")
	check := flag.Bool("check", false, "check recent reconciliation readiness")
	status := flag.Bool("status", false, "show non-secret WireGuard peer status as JSON")
	snapshotPath := flag.String("observation-file", os.Getenv("STATION_GATEWAY_OBSERVATION_FILE"), "optional non-secret interface observation file")
	flag.Parse()
	if *check && *status {
		log.Fatal("check and status are mutually exclusive")
	}
	if *status {
		data, err := os.ReadFile(*configPath)
		if err != nil {
			log.Fatalf("read config: %v", err)
		}
		var configured config
		if err := json.Unmarshal(data, &configured); err != nil {
			log.Fatalf("parse config: %v", err)
		}
		if configured.Interface == "" {
			log.Fatal("config has no interface")
		}
		client, err := wgctrl.New()
		if err != nil {
			log.Fatalf("open wireguard: %v", err)
		}
		defer client.Close()
		device, err := client.Device(configured.Interface)
		if err != nil {
			log.Fatalf("read wireguard device: %v", err)
		}
		if err := writeStatus(os.Stdout, device); err != nil {
			log.Fatalf("write status: %v", err)
		}
		return
	}
	if *check {
		if err := checkReady(*configPath, readinessPath, time.Now()); err != nil {
			log.Printf("not ready: %v", err)
			os.Exit(1)
		}
		return
	}
	if *snapshotPath != "" {
		if err := os.Remove(*snapshotPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Fatalf("clear interface observation: %v", err)
		}
		defer os.Remove(*snapshotPath)
	}
	if err := os.Remove(readinessPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Fatalf("clear readiness: %v", err)
	}
	key, err := keyFile(*keyPath)
	if err != nil {
		log.Fatalf("load identity: %v", err)
	}
	log.Printf("wireguard public key: %s", key.PublicKey())
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		data, e := os.ReadFile(*configPath)
		if e == nil {
			var d *desired
			d, e = parse(data)
			if e == nil {
				e = apply(d, key)
			}
		}
		if e != nil {
			log.Printf("reconcile failed: %v", e)
			if err := os.Remove(readinessPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Printf("clear readiness: %v", err)
			}
		} else if err := writeReady(readinessPath, data, time.Now()); err != nil {
			log.Printf("publish readiness: %v", err)
			if err := os.Remove(readinessPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Printf("clear readiness: %v", err)
			}
		}
		if *snapshotPath != "" {
			if e != nil {
				if err := os.Remove(*snapshotPath); err != nil && !errors.Is(err, os.ErrNotExist) {
					log.Printf("clear interface observation: %v", err)
				}
			} else if err := publishObservation(*snapshotPath, data, time.Now()); err != nil {
				log.Printf("publish interface observation: %v", err)
				if removeErr := os.Remove(*snapshotPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					log.Printf("clear interface observation: %v", removeErr)
				}
			}
		}
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
	}
}
