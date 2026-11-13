package main

import (
	"bytes"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestStatusOmitsSecretsAndShowsHandshake(t *testing.T) {
	private, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	peer, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 11, 1, 2, 3, 0, time.UTC)
	device := &wgtypes.Device{Name: "st-wg0", PrivateKey: private, PublicKey: private.PublicKey(), Peers: []wgtypes.Peer{
		{PublicKey: peer.PublicKey(), PresharedKey: peer, LastHandshakeTime: now, ReceiveBytes: 42, TransmitBytes: 84},
		{PublicKey: private.PublicKey()},
	}}
	var out bytes.Buffer
	if err := writeStatus(&out, device); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), private.String()) || strings.Contains(out.String(), peer.String()) {
		t.Fatal("status leaked a secret")
	}
	var result struct {
		Interface string       `json:"interface"`
		Peers     []peerStatus `json:"peers"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Interface != "st-wg0" || len(result.Peers) != 2 || result.Peers[0].LastHandshake == nil || !result.Peers[0].LastHandshake.Equal(now) || result.Peers[0].ReceiveBytes != 42 || result.Peers[0].TransmitBytes != 84 || result.Peers[1].LastHandshake != nil {
		t.Fatalf("unexpected status: %+v", result)
	}
	if err := writeStatus(&out, nil); err == nil {
		t.Fatal("nil device accepted")
	}
}

func TestObservedPeersCarryKeyHandshakeAndCountersOnly(t *testing.T) {
	private, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	preshared, err := wgtypes.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 14, 1, 2, 3, 0, time.UTC)
	device := &wgtypes.Device{PrivateKey: private, Peers: []wgtypes.Peer{
		{PublicKey: private.PublicKey(), PresharedKey: preshared, Endpoint: &net.UDPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 40000},
			AllowedIPs: []net.IPNet{{IP: net.IPv4(10, 254, 240, 9), Mask: net.CIDRMask(32, 32)}}, LastHandshakeTime: now, ReceiveBytes: 148, TransmitBytes: 92},
		{PublicKey: preshared.PublicKey()},
	}}
	data, err := json.Marshal(observedPeers(device))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"publicKey":"` + private.PublicKey().String() + `","latestHandshake":"2026-11-14T01:02:03Z","receivedBytes":148,"sentBytes":92},{"publicKey":"` + preshared.PublicKey().String() + `"}]`
	if string(data) != want {
		t.Fatalf("peers = %s, want %s", data, want)
	}
}
