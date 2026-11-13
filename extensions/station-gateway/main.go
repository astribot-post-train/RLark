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
	Names        []hostName    `json:"names"`
	DNS          []string      `json:"dns"`
}
type hostName struct {
	Name    string `json:"name"`
	Address string `json:"address"`
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
	for _, name := range c.Names {
		if net.ParseIP(name.Address) == nil || name.Name == "" || strings.ContainsAny(name.Name, " \t\n#") {
			return nil, errors.New("a name requires a host name and an address")
		}
	}
	for _, server := range c.DNS {
		if net.ParseIP(server) == nil {
			return nil, errors.New("a name server is an address")
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

// deviceConfig returns only what differs from the kernel device. An unchanged
// peer is not sent at all, so adding or removing one node never touches the
// sessions of the others; a peer without a configured endpoint keeps the one
// the kernel learned.
func deviceConfig(d *desired, key wgtypes.Key, current *wgtypes.Device) (wgtypes.Config, bool) {
	var cfg wgtypes.Config
	if current.PrivateKey != key {
		cfg.PrivateKey = &key
	}
	if current.ListenPort != d.ListenPort {
		port := d.ListenPort
		cfg.ListenPort = &port
	}
	existing := make(map[wgtypes.Key]wgtypes.Peer, len(current.Peers))
	for _, p := range current.Peers {
		existing[p.PublicKey] = p
	}
	wanted := make(map[wgtypes.Key]bool, len(d.peers))
	for _, want := range d.peers {
		wanted[want.PublicKey] = true
		got, ok := existing[want.PublicKey]
		changed := !ok || want.Endpoint != nil && (got.Endpoint == nil || want.Endpoint.String() != got.Endpoint.String()) ||
			*want.PersistentKeepaliveInterval != got.PersistentKeepaliveInterval
		wantIPs, gotIPs := make([]string, 0, len(want.AllowedIPs)), make([]string, 0, len(got.AllowedIPs))
		for _, ip := range want.AllowedIPs {
			wantIPs = append(wantIPs, ip.String())
		}
		for _, ip := range got.AllowedIPs {
			gotIPs = append(gotIPs, ip.String())
		}
		sort.Strings(wantIPs)
		sort.Strings(gotIPs)
		if changed || strings.Join(wantIPs, ",") != strings.Join(gotIPs, ",") {
			cfg.Peers = append(cfg.Peers, want)
		}
	}
	for _, got := range current.Peers {
		if !wanted[got.PublicKey] {
			cfg.Peers = append(cfg.Peers, wgtypes.PeerConfig{PublicKey: got.PublicKey, Remove: true})
		}
	}
	return cfg, cfg.PrivateKey != nil || cfg.ListenPort != nil || len(cfg.Peers) > 0
}

// apply reconciles the kernel with d. With prune it removes the routes it
// installed on its own interface that d no longer lists; without it such a
// route stops reconciliation until an administrator removes it.
func apply(d *desired, key wgtypes.Key, prune bool) error {
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
	// metric are not proof of ownership on their own; together with the
	// interface this program created and marked they are, and only then, and
	// only when asked to prune, is a route removed.
	existing, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{LinkIndex: link.Attrs().Index, Protocol: routeProtocol}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_PROTOCOL)
	if err != nil {
		return err
	}
	wanted := map[string]bool{}
	var stale []netlink.Route
	for _, dst := range d.routes {
		wanted[dst.String()] = true
	}
	for _, r := range existing {
		if r.Priority == routePriority && r.Dst != nil && !wanted[r.Dst.String()] {
			if !prune {
				return fmt.Errorf("route %s requires manual removal before changing configuration", r.Dst)
			}
			stale = append(stale, r)
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
	for i := range stale {
		if err = netlink.RouteDel(&stale[i]); err != nil {
			return fmt.Errorf("remove route %s: %w", stale[i].Dst, err)
		}
	}
	return nil
}
func main() {
	defaultConfigPath := os.Getenv("STATION_GATEWAY_CONFIG")
	if defaultConfigPath == "" {
		defaultConfigPath = "/config/config.json"
	}
	configPath := flag.String("config", defaultConfigPath, "static configuration written by an administrator")
	computedPath := flag.String("computed-config", os.Getenv("STATION_GATEWAY_COMPUTED_CONFIG"), "optional configuration stored by station-agent from this node's enrollment; preferred over --config")
	identityPath := flag.String("identity-file", os.Getenv("STATION_GATEWAY_IDENTITY_FILE"), "optional file to publish the public key in, for station-agent to enroll")
	prune := flag.Bool("prune-routes", os.Getenv("STATION_GATEWAY_PRUNE_ROUTES") == "true", "remove this program's routes on its own interface when the configuration drops them")
	keyPath := flag.String("key", "/var/lib/station/wg/private.key", "node-local private key")
	check := flag.Bool("check", false, "check recent reconciliation readiness")
	status := flag.Bool("status", false, "show non-secret WireGuard peer status as JSON")
	snapshotPath := flag.String("observation-file", os.Getenv("STATION_GATEWAY_OBSERVATION_FILE"), "optional non-secret interface observation file")
	hostsPath := flag.String("hosts-file", os.Getenv("STATION_GATEWAY_HOSTS_FILE"), "optional hosts file of the node, in which the configuration's names are kept")
	resolvPath := flag.String("resolv-file", os.Getenv("STATION_GATEWAY_RESOLV_FILE"), "optional file in which the configuration's name servers are kept, for the kubelet to give to pods; without any, the machine's own")
	machineResolv := os.Getenv("STATION_GATEWAY_MACHINE_RESOLV")
	if machineResolv == "" {
		machineResolv = "/run/systemd/resolve/resolv.conf,/etc/resolv.conf"
	}
	publicKey := flag.Bool("public-key", false, "create the node's key if it has none, print the public key and exit")
	flag.Parse()
	if *publicKey {
		key, err := keyFile(*keyPath)
		if err != nil {
			log.Fatalf("load identity: %v", err)
		}
		fmt.Println(key.PublicKey())
		return
	}
	if *check && *status {
		log.Fatal("check and status are mutually exclusive")
	}
	from := sources{static: *configPath, computed: *computedPath}
	if *computedPath != "" {
		from.lastApplied = filepath.Join(filepath.Dir(*keyPath), "last-applied.json")
	}
	if *status {
		_, configured, _, err := from.read()
		if err != nil {
			log.Fatalf("read config: %v", err)
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
		data, _, _, err := from.read()
		if err == nil {
			err = checkReady(data, readinessPath, time.Now())
		}
		if err != nil {
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
	// The directory may have been created by the kubelet with a wider mode.
	if err := os.Chmod(filepath.Dir(*keyPath), 0700); err != nil {
		log.Fatalf("protect identity directory: %v", err)
	}
	log.Printf("wireguard public key: %s", key.PublicKey())
	if *identityPath != "" {
		if err := publishIdentity(*identityPath, key); err != nil {
			log.Fatalf("publish identity: %v", err)
		}
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	origin := ""
	for {
		data, d, source, e := from.read()
		if e == nil {
			if source != origin {
				origin = source
				log.Printf("using the %s configuration", source)
			}
			e = apply(d, key, *prune)
		}
		// The tunnel does not depend on these two files: a failure to write
		// one is said, and tried again with the next pass.
		if e == nil && *hostsPath != "" {
			if err := writeNames(*hostsPath, d.Names); err != nil {
				log.Printf("keep the names in %s: %v", *hostsPath, err)
			}
		}
		if e == nil && *resolvPath != "" {
			if err := writeResolv(*resolvPath, d.DNS, strings.Split(machineResolv, ",")); err != nil {
				log.Printf("keep the name servers in %s: %v", *resolvPath, err)
			}
		}
		if e == nil && source == "computed" {
			if saved, readErr := os.ReadFile(from.lastApplied); readErr != nil || string(saved) != string(data) {
				if err := replaceFile(from.lastApplied, data, 0600); err != nil {
					log.Printf("keep last applied configuration: %v", err)
				}
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
