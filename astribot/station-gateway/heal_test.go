package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// An outage takes the uplink, then the device, then asks for a restart, each
// once; restarts that do not hold the tunnel wait twice as long each time.
func TestLinkStepsUpThroughAnOutage(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	l := link{State: "up", Since: start.Unix(), RestartOwner: "s1u-robot"}
	at := func(d time.Duration) time.Time { return start.Add(d) }
	steps := func(d time.Duration, answered bool, keepUplink string) string {
		step := l.next(answered, at(d), keepUplink)
		if step == "restart" {
			l.Restarts++
			l.Restart = &restartRequest{ID: l.Restarts, At: at(d).Unix()}
		}
		if step != "" {
			l.Done = append(l.Done, step)
		}
		return step
	}
	for _, want := range []struct {
		at       time.Duration
		answered bool
		step     string
	}{
		{0, true, ""},
		{time.Minute, false, "uplink"},
		{90 * time.Second, false, ""},
		{2 * time.Minute, false, "wireguard"},
		{3 * time.Minute, false, ""},
		{4 * time.Minute, false, "restart"},
		{10 * time.Minute, false, ""},
		{19 * time.Minute, false, "restart"}, // 15 min after the first
		{40 * time.Minute, false, ""},
		{49 * time.Minute, false, "restart"}, // 30 min after the second
		{50 * time.Minute, true, ""},
	} {
		if got := steps(want.at, want.answered, ""); got != want.step {
			t.Fatalf("at %s: step %q, want %q (%+v)", want.at, got, want.step, l)
		}
	}
	if l.State != "up" || l.Restarts != 3 || l.Actions[len(l.Actions)-1].Step != "recovered" {
		t.Fatalf("after recovery: %+v", l)
	}
	// Up for half an hour: the next outage restarts as soon as the first.
	steps(81*time.Minute, true, "")
	if l.Restarts != 0 {
		t.Fatalf("restarts not forgotten: %+v", l)
	}
	// An uplink kept as it is (whole, or the machine in use): the device is still made anew.
	if got := steps(90*time.Minute, false, "the machine is in use"); got != "" || l.Actions[len(l.Actions)-1].Result != "skipped: the machine is in use" {
		t.Fatalf("in use: step %q, %+v", got, l.Actions)
	}
	if got := steps(91*time.Minute, false, "x"); got != "wireguard" {
		t.Fatalf("in use: step %q, want wireguard", got)
	}
	if got := steps(93*time.Minute, false, "x"); got != "restart" {
		t.Fatalf("in use: step %q, want restart", got)
	}
}

// A machine nothing owns the restart of (a workstation, a GPU node) is not
// asked of: the record says once per outage that a person restarts it.
func TestLinkAsksNoRestartOfAMachineWithoutAnOwner(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	l := link{State: "up", Since: start.Unix()}
	for _, d := range []time.Duration{0, 4 * time.Minute, 19 * time.Minute, 49 * time.Minute} {
		if step := l.next(false, start.Add(d), "whole"); step != "" && step != "wireguard" {
			t.Fatalf("at %s: step %q", d, step)
		} else if step != "" {
			l.Done = append(l.Done, step)
		}
	}
	var unowned []healAction
	for _, action := range l.Actions {
		if action.Step == "unowned" || action.Step == "restart" {
			unowned = append(unowned, action)
		}
	}
	if len(unowned) != 1 || unowned[0].Step != "unowned" || l.Restarts != 0 || l.Restart != nil {
		t.Fatalf("without an owner: %+v", l)
	}
}

func TestLinkKeepsTheLastActions(t *testing.T) {
	var l link
	for i := range 25 {
		l.record(time.Unix(int64(i), 0), "down", "")
	}
	if len(l.Actions) != healActions || l.Actions[0].At != 15 {
		t.Fatalf("actions %+v", l.Actions)
	}
}

// Station API unanswered is said once and once again when it answers; a node
// that does not ask says nothing.
func TestLinkHearsStationAPI(t *testing.T) {
	started := time.Unix(1_800_000_000, 0)
	var l link
	l.heard(false, time.Time{}, started.Add(time.Hour), started)
	if len(l.Actions) != 0 {
		t.Fatalf("not asked: %+v", l)
	}
	l.heard(true, time.Time{}, started.Add(time.Minute), started)
	l.heard(true, time.Time{}, started.Add(4*time.Minute), started)
	l.heard(true, time.Time{}, started.Add(5*time.Minute), started)
	l.heard(true, started.Add(6*time.Minute), started.Add(6*time.Minute), started)
	if len(l.Actions) != 2 || l.RLarkLost || l.RLark != started.Add(6*time.Minute).Unix() || l.Actions[1].Result != "Station API answers again" {
		t.Fatalf("heard: %+v", l)
	}
}

// pass reads the owner from restart-owner beside link.json and keeps it in
// the file: asked of the owner, or recorded for a person without one.
func TestPassAsksTheRestartOfTheDeclaredOwner(t *testing.T) {
	d := &desired{peers: []wgtypes.PeerConfig{{Endpoint: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 51820}}}}
	d.Interface = "st-test-none"
	start := time.Unix(1_800_000_000, 0)
	for _, owner := range []string{"s1u-robot\n", ""} {
		directory := t.TempDir()
		if owner != "" {
			if err := os.WriteFile(filepath.Join(directory, "restart-owner"), []byte(owner), 0644); err != nil {
				t.Fatal(err)
			}
		}
		h := newHealer(filepath.Join(directory, "link.json"), start)
		for _, after := range []time.Duration{handshakeAge, handshakeAge + healTunnelAfter, handshakeAge + healRestartAfter} {
			h.pass(d, nil, start.Add(after))
		}
		data, err := os.ReadFile(h.path)
		var kept link
		if err == nil {
			err = json.Unmarshal(data, &kept)
		}
		if err != nil {
			t.Fatal(err)
		}
		last := kept.Actions[len(kept.Actions)-1]
		switch {
		case owner != "" && (kept.RestartOwner != "s1u-robot" || kept.Restart == nil || !strings.HasPrefix(last.Result, "restart 1 asked of s1u-robot")):
			t.Fatalf("with an owner: %+v", kept)
		case owner == "" && (kept.RestartOwner != "" || kept.Restart != nil || last.Step != "unowned"):
			t.Fatalf("without an owner: %+v", kept)
		}
	}
}
