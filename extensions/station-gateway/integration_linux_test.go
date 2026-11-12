//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Run only in a disposable network namespace with CAP_NET_ADMIN.
func TestKernelReconciliation(t *testing.T) {
	if os.Getenv("STATION_NETNS_TEST") != "1" {
		t.Skip("requires disposable privileged network namespace")
	}
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	d, err := parse([]byte(`{"interface":"wg-st-test","address":"10.253.0.1/32","listenPort":51820,"mtu":1420,"routes":["10.253.0.2/32"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = apply(d, key); err != nil {
		t.Fatal(err)
	}
	if err = apply(d, key); err != nil {
		t.Fatalf("restart reconcile: %v", err)
	}
	changed, err := parse([]byte(`{"interface":"wg-st-test","address":"10.253.0.9/32","listenPort":51821,"mtu":1420}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = apply(changed, key); err == nil {
		t.Fatal("accepted address migration on existing interface")
	}
	linkBefore, err := netlink.LinkByName("wg-st-test")
	if err != nil {
		t.Fatal(err)
	}
	addresses, err := netlink.AddrList(linkBefore, netlink.FAMILY_V4)
	if err != nil || len(addresses) != 1 || addresses[0].IPNet.String() != "10.253.0.1/32" {
		t.Fatalf("address changed after rejected migration: %v %v", addresses, err)
	}
	if err = apply(d, key); err != nil {
		t.Fatalf("recover original configuration: %v", err)
	}
	uplink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "station-uplink"}}
	if err = netlink.LinkAdd(uplink); err != nil {
		t.Fatal(err)
	}
	addr, _ := netlink.ParseAddr("192.0.2.10/24")
	if err = netlink.AddrAdd(uplink, addr); err != nil {
		t.Fatal(err)
	}
	if err = netlink.LinkSetUp(uplink); err != nil {
		t.Fatal(err)
	}
	d.UplinkRoutes = []uplinkRoute{{Destination: "198.51.100.1/32", Gateway: "192.0.2.1", Source: "192.0.2.10", Interface: "station-uplink"}}
	if err = apply(d, key); err != nil {
		t.Fatalf("add uplink: %v", err)
	}
	if err = apply(d, key); err != nil {
		t.Fatalf("reconcile uplink: %v", err)
	}
	d.UplinkRoutes[0].Source = "192.0.2.11"
	if err = apply(d, key); err == nil {
		t.Fatal("accepted missing uplink source")
	}
	d.UplinkRoutes[0].Source = "192.0.2.10"
	_, uplinkDst, _ := net.ParseCIDR("198.51.100.1/32")
	conflict := netlink.Route{LinkIndex: uplink.Attrs().Index, Dst: uplinkDst, Gw: net.ParseIP("192.0.2.1"), Src: net.ParseIP("192.0.2.10"), Protocol: 99, Priority: routePriority, Table: 254}
	if err = netlink.RouteReplace(&conflict); err != nil {
		t.Fatal(err)
	}
	if err = apply(d, key); err == nil {
		t.Fatal("accepted foreign uplink route")
	}
	d.UplinkRoutes = nil
	link, err := netlink.LinkByName("wg-st-test")
	if err != nil {
		t.Fatal(err)
	}
	_, dst, _ := net.ParseCIDR("10.253.0.3/32")
	other := netlink.Route{LinkIndex: link.Attrs().Index, Dst: dst, Protocol: routeProtocol, Priority: routePriority, Table: 254, Type: 1}
	if err = netlink.RouteAdd(&other); err != nil {
		t.Fatal(err)
	}
	if err = apply(d, key); err == nil {
		t.Fatal("accepted ambiguous pre-existing route")
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Dst: dst}, netlink.RT_FILTER_DST)
	if err != nil || len(routes) != 1 {
		t.Fatalf("removed external route: %v %v", routes, err)
	}
	foreign := &netlink.GenericLink{LinkAttrs: netlink.LinkAttrs{Name: "wg-foreign"}, LinkType: "wireguard"}
	if err = netlink.LinkAdd(foreign); err != nil {
		t.Fatal(err)
	}
	d.Interface = "wg-foreign"
	if err = apply(d, key); err == nil {
		t.Fatal("adopted unowned interface")
	}
}

func TestKernelStatus(t *testing.T) {
	if os.Getenv("STATION_NETNS_TEST") != "1" {
		t.Skip("requires disposable privileged network namespace")
	}
	private, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	peerKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	d, err := parse([]byte(`{"interface":"wg-status","address":"10.253.5.1/32","listenPort":51822,"mtu":1420,"peers":[{"publicKey":"` + peerKey.PublicKey().String() + `","allowedIPs":["10.253.5.2/32"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := apply(d, private); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "observation.json")
	configData, err := json.Marshal(d.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := publishObservation(snapshot, configData, time.Now()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var observation struct {
		Interface string   `json:"interface"`
		PublicKey string   `json:"publicKey"`
		Addresses []string `json:"addresses"`
	}
	if err := json.Unmarshal(data, &observation); err != nil {
		t.Fatal(err)
	}
	if observation.Interface != "wg-status" || observation.PublicKey != private.PublicKey().String() || len(observation.Addresses) == 0 || observation.Addresses[0] != "10.253.5.1/32" {
		t.Fatalf("unexpected snapshot: %s", data)
	}
	if bytes.Contains(data, []byte(private.String())) || bytes.Contains(data, []byte(peerKey.PublicKey().String())) {
		t.Fatal("snapshot contains a key")
	}
	client, err := wgctrl.New()
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Device("wg-absent"); err == nil {
		t.Fatal("missing device accepted")
	}
	device, err := client.Device("wg-status")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := writeStatus(&out, device); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Interface string       `json:"interface"`
		PublicKey string       `json:"publicKey"`
		Peers     []peerStatus `json:"peers"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Interface != "wg-status" || got.PublicKey != private.PublicKey().String() || len(got.Peers) != 1 || got.Peers[0].PublicKey != peerKey.PublicKey().String() || got.Peers[0].LastHandshake != nil || got.Peers[0].ReceiveBytes != 0 || got.Peers[0].TransmitBytes != 0 {
		t.Fatalf("unexpected kernel status: %+v", got)
	}
}
