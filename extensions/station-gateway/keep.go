package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// keeper keeps the station-gateway a machine runs as a service of its own
// (rlark-station-gateway.service, put there once by the installer) at the
// version of the image this program was started from. It runs in the node's
// station-agent-host pod, so the image of that DaemonSet is the declaration
// for every such node of the cluster at once.
//
// Replacing the program does not touch the tunnel: the device and its sessions
// are the kernel's, the service only reconciles them. The service is told to
// end and its unit (Restart=always) starts the new file. A new program that
// does not stay up and reconcile within the wait is replaced by the one
// before it, and that version is not tried again on this node.
type keeper struct {
	self        string // this program's own file
	target      string // the machine's program as this container sees it
	name        string // the same file as the machine's unit names it
	hold        string // a directory of the machine; a file in it says the machine is in use
	observation string // the file the service rewrites with every good pass
	proc        string
	wait, stay  time.Duration
	signal      func(pid int) error
	sleep       func(time.Duration)
	now         func() time.Time
}

func newKeeper(target, name, hold, observation string) (*keeper, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return &keeper{self: self, target: target, name: name, hold: hold, observation: observation, proc: "/proc",
		wait: 2 * time.Minute, stay: 30 * time.Second,
		signal: func(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }, sleep: time.Sleep, now: time.Now}, nil
}

func digest(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:]), nil
}

// kept reports whether the machine runs this program's version.
func (k *keeper) kept() (bool, error) {
	own, err := digest(k.self)
	if err != nil {
		return false, err
	}
	theirs, err := digest(k.target)
	return own == theirs, err
}

// running lists the service's processes: pid and whether the file it was
// started from is still the one at its path.
func (k *keeper) running() map[int]bool {
	found := map[int]bool{}
	entries, _ := os.ReadDir(k.proc)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join(k.proc, entry.Name(), "cmdline"))
		if err != nil || string(bytes.SplitN(cmdline, []byte{0}, 2)[0]) != k.name {
			continue
		}
		exe, _ := os.Readlink(filepath.Join(k.proc, entry.Name(), "exe"))
		found[pid] = exe == k.name
	}
	return found
}

func (k *keeper) put(from string) error {
	content, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	if err = replaceFile(k.target, content, 0755); err != nil {
		return err
	}
	for pid := range k.running() {
		if err := k.signal(pid); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("end process %d: %w", pid, err)
		}
	}
	return nil
}

// pass does one round and says what state the machine is left in.
func (k *keeper) pass() (string, error) {
	same, err := k.kept()
	if err != nil || same {
		return "kept", err
	}
	own, _ := digest(k.self)
	if rejected, _ := os.ReadFile(k.target + ".rejected"); strings.TrimSpace(string(rejected)) == own {
		return "rejected", nil
	}
	if entries, _ := os.ReadDir(k.hold); len(entries) > 0 {
		return "held: " + entries[0].Name(), nil
	}
	previous := k.target + ".previous"
	content, err := os.ReadFile(k.target)
	if err != nil {
		return "", err
	}
	if err = replaceFile(previous, content, 0755); err != nil {
		return "", err
	}
	since := k.now()
	if err = k.put(k.self); err != nil {
		return "", err
	}
	if reason := k.settled(since); reason != "" {
		if err = os.WriteFile(k.target+".rejected", []byte(own+"\n"), 0644); err != nil {
			return "", err
		}
		if err = k.put(previous); err != nil {
			return "", fmt.Errorf("%s, and the program before it could not be put back: %w", reason, err)
		}
		return "rejected", errors.New(reason + ": the program before it is back")
	}
	return "replaced", nil
}

// settled waits for one process of the new file that stays up and reconciles,
// and returns why there is none.
func (k *keeper) settled(since time.Time) string {
	first, seen := 0, time.Time{}
	for k.now().Sub(since) < k.wait {
		k.sleep(time.Second)
		alive := false
		for pid, current := range k.running() {
			if !current {
				continue
			}
			if pid != first {
				first, seen = pid, k.now()
			}
			alive = true
		}
		if !alive {
			first = 0
			continue
		}
		observed, err := os.Stat(k.observation)
		if k.now().Sub(seen) >= k.stay && err == nil && observed.ModTime().After(seen) {
			return ""
		}
	}
	if first == 0 {
		return "the new program was not running after " + k.wait.String()
	}
	return "the new program did not stay up and reconcile within " + k.wait.String()
}

func (k *keeper) run(stop <-chan os.Signal) {
	state := ""
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		next, err := k.pass()
		if err != nil {
			log.Printf("keep %s: %v", k.name, err)
		}
		if next != state && next != "" {
			state = next
			log.Printf("%s: %s", k.name, state)
		}
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
	}
}
