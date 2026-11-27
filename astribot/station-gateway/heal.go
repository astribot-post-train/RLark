package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// The node looks for RLark itself, whether or not RLark still sees it: every
// 30 s it asks Station API for its own document (ownDocument), and every pass
// it reads the handshake of the peer its way to Station API runs through.
// The tunnel is down when that peer has not answered for handshakeAge (a node
// that has no station-api.json: when no peer it dials has): a hub that is not
// on the way to RLark is that hub's matter. Station API unanswered over a
// tunnel that answers is the control plane's matter, not this node's: it is
// recorded and nothing is done, or a control plane that restarts would
// restart every robot of the site. A node whose tunnel is down works its way up, once each per outage: its uplink
// taken up again when it has lost its address or its route to the hub (the
// network manager renews the lease or puts the static address and routes
// back; an uplink that has both is left alone, a GPU node's among them, whose
// training would feel it), the WireGuard device made anew (another socket and
// port, every session from scratch), and last a restart of the machine. The
// restart is asked for, not done: link.json says so, and what owns the
// machine's restart does it when it can do it safely (s1u-robot's tuner: the
// driver stopped, the arms let sink, the motors off, then the whole robot).
// The owner declares itself in restart-owner beside link.json. A machine
// without one (a workstation, a GPU node) is not asked of and not restarted,
// and the record says a person restarts it: a node cut off cannot be drained,
// nothing marks the RLark Tasks it runs as a use, and a workstation may hold
// its site's control plane. While a restart did not
// bring the tunnel back for good, the next waits twice as long as the one
// before it, so a site whose own network is gone does not keep its robots
// restarting.
const (
	healTunnelAfter  = time.Minute
	healRestartAfter = 3 * time.Minute
	healRestartFirst = 15 * time.Minute // the wait after the first restart, doubled after each
	healRestartLast  = 4 * time.Hour
	healSettled      = 30 * time.Minute // up this long: the restarts before are forgotten
	healActions      = 10
	inUseDirectory   = "/run/rlark/in-use"
)

type healAction struct {
	At     int64  `json:"at"`
	Step   string `json:"step"`
	Result string `json:"result"`
}

type restartRequest struct {
	ID  int    `json:"id"`
	At  int64  `json:"at"`
	Why string `json:"why"`
}

// link is the file (link.json beside the node's key, kept across restarts):
// the tunnel's state since when, the uplink it was last reached through,
// the steps taken in this outage, the restart asked for last, and the last
// actions. Times are Unix seconds.
type link struct {
	State string `json:"state"`
	// RLark is when Station API last answered this node (0: not since
	// this program started); RLarkLost, whether it has not for handshakeAge.
	RLark     int64  `json:"rlark,omitempty"`
	RLarkLost bool   `json:"rlarkLost,omitempty"`
	Since     int64  `json:"since"`
	Uplink    string `json:"uplink,omitempty"`
	// RestartOwner is what restart-owner beside the file names ("": nothing
	// on the machine restarts it), kept here so RLark knows it before an outage.
	RestartOwner string          `json:"restartOwner,omitempty"`
	Done         []string        `json:"done,omitempty"`
	Restarts     int             `json:"restarts"`
	Restart      *restartRequest `json:"restart,omitempty"`
	Actions      []healAction    `json:"actions"`
}

// next is the step the tunnel's state calls for now, "" for none. answered
// says whether a dialled peer answered within handshakeAge; a node that dials
// no one is never down. keepUplink, when not "", is why the uplink is not
// taken up again.
func (l *link) next(answered bool, now time.Time, keepUplink string) string {
	if answered {
		if l.State == "down" {
			l.record(now, "recovered", fmt.Sprintf("the tunnel answers again after %s", time.Duration(now.Unix()-l.Since)*time.Second))
		}
		if l.State != "up" {
			l.State, l.Since, l.Done = "up", now.Unix(), nil
		}
		if l.Restarts > 0 && now.Unix()-l.Since >= int64(healSettled/time.Second) {
			l.Restarts = 0
		}
		return ""
	}
	if l.State != "down" {
		l.State, l.Since, l.Done = "down", now.Unix(), nil
		l.record(now, "down", fmt.Sprintf("no peer this node dials has answered for %s", handshakeAge))
	}
	elapsed := now.Sub(time.Unix(l.Since, 0))
	switch {
	case !slices.Contains(l.Done, "uplink"):
		if keepUplink != "" {
			l.Done = append(l.Done, "uplink")
			l.record(now, "uplink", "skipped: "+keepUplink)
			return ""
		}
		return "uplink"
	case elapsed >= healTunnelAfter && !slices.Contains(l.Done, "wireguard"):
		return "wireguard"
	case elapsed >= healRestartAfter && l.RestartOwner == "":
		if !slices.Contains(l.Done, "restart") {
			l.Done = append(l.Done, "restart")
			l.record(now, "unowned", "nothing on this machine owns its restart (restart-owner); a person restarts it if it needs one")
		}
		return ""
	case elapsed >= healRestartAfter && (l.Restart == nil || now.Sub(time.Unix(l.Restart.At, 0)) >= l.restartWait()):
		return "restart"
	}
	return ""
}

// heard keeps what the node knows of Station API: answered at reached (zero:
// not yet), asked at all (a node without station-api.json is not).
func (l *link) heard(asked bool, reached, now, started time.Time) {
	if !asked {
		return
	}
	if !reached.IsZero() {
		l.RLark = reached.Unix()
	}
	last := started
	if reached.After(started) {
		last = reached
	}
	lost := now.Sub(last) > handshakeAge
	switch {
	case lost && !l.RLarkLost:
		l.record(now, "rlark", fmt.Sprintf("Station API has not answered for %s", handshakeAge))
	case !lost && l.RLarkLost:
		l.record(now, "rlark", "Station API answers again")
	}
	l.RLarkLost = lost
}

// restartWait is how long after the last restart asked for another may be.
func (l *link) restartWait() time.Duration {
	if l.Restarts == 0 {
		return 0
	}
	return min(healRestartFirst<<(l.Restarts-1), healRestartLast)
}

func (l *link) record(now time.Time, step, result string) {
	log.Printf("link: %s: %s", step, result)
	l.Actions = append(l.Actions, healAction{At: now.Unix(), Step: step, Result: result})
	l.Actions = l.Actions[max(0, len(l.Actions)-healActions):]
}

// healer keeps the file and takes the steps.
type healer struct {
	path    string
	started time.Time
	link
}

func newHealer(path string, now time.Time) *healer {
	h := &healer{path: path, started: now, link: link{State: "up", Since: now.Unix()}}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &h.link); err != nil {
			log.Printf("link: %s is unreadable, starting afresh: %v", path, err)
		}
	}
	return h
}

// pass reads the tunnel and takes the step it calls for. current is nil when
// the device is not there.
func (h *healer) pass(d *desired, current *wgtypes.Device, now time.Time) {
	var dialled []net.IP
	watched := map[wgtypes.Key]bool{}
	for _, p := range d.peers {
		if p.Endpoint != nil {
			dialled = append(dialled, p.Endpoint.IP)
			watched[p.PublicKey] = true
		}
	}
	if len(dialled) == 0 || now.Sub(h.started) < handshakeAge {
		return // a hub, or too soon after this program started to tell
	}
	askedAPI.Lock()
	url, reached := askedAPI.url, askedAPI.reached
	askedAPI.Unlock()
	h.heard(url != "", reached, now, h.started)
	if peer, ok := peerTo(url, d); ok {
		watched = map[wgtypes.Key]bool{peer: true}
	}
	answered := false
	if current != nil {
		for _, p := range current.Peers {
			answered = answered || watched[p.PublicKey] && !p.LastHandshakeTime.IsZero() && now.Sub(p.LastHandshakeTime) < handshakeAge
		}
	}
	// The uplink is whole when the hub has a route through a link that has
	// an IPv4 address; whole and answered, it is the one remembered.
	keepUplink := ""
	if routes, err := netlink.RouteGet(dialled[0]); err == nil && len(routes) > 0 {
		if l, err := netlink.LinkByIndex(routes[0].LinkIndex); err == nil {
			if addresses, err := netlink.AddrList(l, netlink.FAMILY_V4); err == nil && len(addresses) > 0 {
				keepUplink = fmt.Sprintf("%s has its address and a route to %s", l.Attrs().Name, dialled[0])
				if answered {
					h.Uplink = l.Attrs().Name
				}
			}
		}
	}
	if entries, _ := os.ReadDir(inUseDirectory); keepUplink == "" && len(entries) > 0 {
		// Taking the uplink down and up moves the machine's own network (a
		// robot's x86 and its Orin talk over it): not under a use.
		keepUplink = "the machine is in use"
	}
	declared, _ := os.ReadFile(filepath.Join(filepath.Dir(h.path), "restart-owner"))
	h.RestartOwner = string(bytes.TrimSpace(declared))
	step := h.next(answered, now, keepUplink)
	switch step {
	case "uplink":
		h.record(now, step, uplinkAgain(h.Uplink))
	case "wireguard":
		h.record(now, step, deviceAgain(d.Interface))
	case "restart":
		h.Restarts++
		id := 1
		if h.Restart != nil {
			id = h.Restart.ID + 1
		}
		h.Restart = &restartRequest{ID: id, At: now.Unix(), Why: fmt.Sprintf("the tunnel has not answered since %s", time.Unix(h.Since, 0).UTC().Format(time.RFC3339))}
		h.record(now, step, fmt.Sprintf("restart %d asked of %s (the next no sooner than %s later)", id, h.RestartOwner, h.restartWait()))
	}
	if step != "" {
		h.Done = append(h.Done, step)
	}
	data, err := json.Marshal(h.link)
	if err == nil {
		err = replaceFile(h.path, bytes.NewReader(data), 0644)
	}
	if err != nil {
		log.Printf("link: keep %s: %v", h.path, err)
	}
}

// askedAPI is what the node's own asking (askStationAPI) last saw: Station
// API's address and when it last answered, whatever it answered.
var askedAPI struct {
	sync.Mutex
	url     string
	reached time.Time
}

// peerTo is the peer whose allowed addresses hold Station API's.
func peerTo(url string, d *desired) (wgtypes.Key, bool) {
	parsed, err := neturl.Parse(url)
	if err != nil || parsed.Hostname() == "" {
		return wgtypes.Key{}, false
	}
	addresses, err := net.LookupIP(parsed.Hostname())
	if err != nil {
		return wgtypes.Key{}, false
	}
	for _, p := range d.peers {
		for _, allowed := range p.AllowedIPs {
			for _, address := range addresses {
				if allowed.Contains(address) {
					return p.PublicKey, true
				}
			}
		}
	}
	return wgtypes.Key{}, false
}

// uplinkAgain takes the uplink up again through whatever manages it, so its
// address and routes come back as that manager has them: NetworkManager
// (robots, workstations), systemd-networkd, else the link itself down and up.
func uplinkAgain(name string) string {
	if name == "" {
		return "skipped: the uplink was never seen"
	}
	for _, command := range [][]string{{"nmcli", "device", "up", name}, {"networkctl", "reconfigure", name}} {
		if _, err := exec.LookPath(command[0]); err != nil {
			continue
		}
		output, err := exec.Command(command[0], command[1:]...).CombinedOutput()
		if err == nil {
			return fmt.Sprintf("%s: %s", command[0], bytes.TrimSpace(output))
		}
		log.Printf("link: %v: %v: %s", command, err, bytes.TrimSpace(output))
	}
	l, err := netlink.LinkByName(name)
	if err == nil {
		err = netlink.LinkSetDown(l)
	}
	if err == nil {
		err = netlink.LinkSetUp(l)
	}
	if err != nil {
		return fmt.Sprintf("%s down and up: %v", name, err)
	}
	return name + " down and up"
}

// deviceAgain removes the WireGuard device this program made; the pass that
// follows makes it anew from the document.
func deviceAgain(name string) string {
	l, err := netlink.LinkByName(name)
	var notFound netlink.LinkNotFoundError
	switch {
	case errors.As(err, &notFound):
		return "no device to make anew"
	case err != nil:
		return err.Error()
	case l.Type() != "wireguard" || l.Attrs().Alias != owner:
		return fmt.Sprintf("%s is not this program's", name)
	}
	if err := netlink.LinkDel(l); err != nil {
		return err.Error()
	}
	dialling = redial{}
	return name + " removed, made anew by this pass"
}
