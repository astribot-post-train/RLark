package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// peerObservation is what a hub knows of a node that has dropped off: whether
// a session formed and whether anything still arrives.
type peerObservation struct {
	PublicKey       string     `json:"publicKey"`
	LatestHandshake *time.Time `json:"latestHandshake,omitempty"`
	ReceivedBytes   int64      `json:"receivedBytes,omitempty"`
	SentBytes       int64      `json:"sentBytes,omitempty"`
}

func observedPeers(device *wgtypes.Device) []peerObservation {
	peers := make([]peerObservation, 0, len(device.Peers))
	for _, peer := range device.Peers {
		observed := peerObservation{PublicKey: peer.PublicKey.String(), ReceivedBytes: peer.ReceiveBytes, SentBytes: peer.TransmitBytes}
		if !peer.LastHandshakeTime.IsZero() {
			handshake := peer.LastHandshakeTime.UTC()
			observed.LatestHandshake = &handshake
		}
		peers = append(peers, observed)
	}
	return peers
}

// publishObservation writes the interface, its addresses and, for every peer,
// its public key, the time of its last handshake and the bytes received from
// and sent to it. It never includes the device's keys, a preshared key, an
// endpoint or allowed addresses.
func publishObservation(path string, data []byte, now time.Time) error {
	d, err := parse(data)
	if err != nil {
		return err
	}
	link, err := netlink.LinkByName(d.Interface)
	if err != nil {
		return err
	}
	if link.Type() != "wireguard" || link.Attrs().Alias != owner {
		return fmt.Errorf("interface is not owned by station gateway")
	}
	addresses, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return err
	}
	values := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if address.IPNet != nil {
			values = append(values, address.IPNet.String())
		}
	}
	sort.Strings(values)
	client, err := wgctrl.New()
	if err != nil {
		return err
	}
	defer client.Close()
	device, err := client.Device(d.Interface)
	if err != nil {
		return err
	}
	content, err := json.Marshal(struct {
		ObservedAt time.Time         `json:"observedAt"`
		Interface  string            `json:"interface"`
		Addresses  []string          `json:"addresses"`
		Peers      []peerObservation `json:"peers"`
	}{now.UTC(), d.Interface, values, observedPeers(device)})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".station-observation-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0644); err == nil {
		_, err = f.Write(content)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

type peerStatus struct {
	PublicKey     string     `json:"publicKey"`
	LastHandshake *time.Time `json:"lastHandshake"`
	ReceiveBytes  int64      `json:"receiveBytes"`
	TransmitBytes int64      `json:"transmitBytes"`
}

// writeStatus deliberately projects only non-secret operational fields.
func writeStatus(w io.Writer, device *wgtypes.Device) error {
	if device == nil {
		return fmt.Errorf("wireguard device is unavailable")
	}
	peers := make([]peerStatus, 0, len(device.Peers))
	for _, peer := range device.Peers {
		var handshake *time.Time
		if !peer.LastHandshakeTime.IsZero() {
			value := peer.LastHandshakeTime.UTC()
			handshake = &value
		}
		peers = append(peers, peerStatus{
			PublicKey: peer.PublicKey.String(), LastHandshake: handshake,
			ReceiveBytes: peer.ReceiveBytes, TransmitBytes: peer.TransmitBytes,
		})
	}
	return json.NewEncoder(w).Encode(struct {
		Interface string       `json:"interface"`
		PublicKey string       `json:"publicKey"`
		Peers     []peerStatus `json:"peers"`
	}{device.Name, device.PublicKey.String(), peers})
}
