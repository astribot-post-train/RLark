package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// machine is a host as the keeper sees it: the program's file, /proc, and a
// unit that starts the file at its path again when its process is ended.
type machine struct {
	t       *testing.T
	k       *keeper
	clock   time.Time
	pid     int
	healthy func(program string) bool
}

func newMachine(t *testing.T, own, theirs string) *machine {
	dir := t.TempDir()
	m := &machine{t: t, clock: time.Unix(1_800_000_000, 0), pid: 100, healthy: func(string) bool { return true }}
	for name, content := range map[string]string{"self": own, "station-gateway": theirs} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, sub := range []string{"proc", "hold"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0755); err != nil {
			t.Fatal(err)
		}
	}
	m.k = &keeper{self: filepath.Join(dir, "self"), target: filepath.Join(dir, "station-gateway"), name: "/opt/rlark/bin/station-gateway",
		hold: filepath.Join(dir, "hold"), observation: filepath.Join(dir, "observation"), proc: filepath.Join(dir, "proc"),
		wait: 2 * time.Minute, stay: 30 * time.Second, now: func() time.Time { return m.clock }}
	m.k.signal = func(pid int) error {
		os.RemoveAll(filepath.Join(m.k.proc, strconv.Itoa(pid)))
		m.pid++
		m.start()
		return nil
	}
	m.k.sleep = func(d time.Duration) {
		m.clock = m.clock.Add(d)
		if m.healthy(m.program()) {
			os.WriteFile(m.k.observation, nil, 0644)
			os.Chtimes(m.k.observation, m.clock, m.clock)
		} else if m.clock.Second()%10 == 0 {
			// A program that exits is started again by its unit, under a new pid.
			m.k.signal(m.pid)
		}
	}
	m.start()
	return m
}

func (m *machine) program() string {
	content, _ := os.ReadFile(m.k.target)
	return string(content)
}

func (m *machine) start() {
	dir := filepath.Join(m.k.proc, strconv.Itoa(m.pid))
	os.Mkdir(dir, 0755)
	os.WriteFile(filepath.Join(dir, "cmdline"), []byte(m.k.name+"\x00--config\x00x"), 0644)
	os.Symlink(m.k.name, filepath.Join(dir, "exe"))
}

func TestKeeperReplacesTheMachinesProgramAndLeavesItAloneAfterwards(t *testing.T) {
	m := newMachine(t, "new", "old")
	if state, err := m.k.pass(); state != "replaced" || err != nil || m.program() != "new" || m.pid != 101 {
		t.Fatalf("state %q err %v program %q pid %d", state, err, m.program(), m.pid)
	}
	if previous, _ := os.ReadFile(m.k.target + ".previous"); string(previous) != "old" {
		t.Fatalf("the program before it was not kept: %q", previous)
	}
	if state, err := m.k.pass(); state != "kept" || err != nil || m.pid != 101 {
		t.Fatalf("a kept program was touched: %q %v pid %d", state, err, m.pid)
	}
}

func TestKeeperWaitsWhileTheMachineIsInUse(t *testing.T) {
	m := newMachine(t, "new", "old")
	held := filepath.Join(m.k.hold, "robot-powered")
	os.WriteFile(held, nil, 0644)
	if state, err := m.k.pass(); state != "held: robot-powered" || err != nil || m.program() != "old" || m.pid != 100 {
		t.Fatalf("state %q err %v program %q pid %d", state, err, m.program(), m.pid)
	}
	os.Remove(held)
	if state, _ := m.k.pass(); state != "replaced" {
		t.Fatalf("state %q after the machine was free", state)
	}
}

func TestKeeperPutsTheProgramBackWhenTheNewOneDoesNotStayUp(t *testing.T) {
	m := newMachine(t, "new", "old")
	m.healthy = func(program string) bool { return program == "old" }
	state, err := m.k.pass()
	if state != "rejected" || err == nil || !strings.Contains(err.Error(), "did not stay up") || m.program() != "old" {
		t.Fatalf("state %q err %v program %q", state, err, m.program())
	}
	pid := m.pid
	if state, err := m.k.pass(); state != "rejected" || err != nil || m.pid != pid || m.program() != "old" {
		t.Fatalf("a rejected version was tried again: %q %v", state, err)
	}
	// Another version is another attempt.
	os.WriteFile(m.k.self, []byte("newer"), 0755)
	m.healthy = func(string) bool { return true }
	if state, err := m.k.pass(); state != "replaced" || err != nil || m.program() != "newer" {
		t.Fatalf("state %q err %v program %q", state, err, m.program())
	}
}
