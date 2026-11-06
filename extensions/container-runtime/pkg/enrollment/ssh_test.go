package enrollment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/core/api/v1alpha1"
)

func TestSSHConnectionReuseKeepsStrictTrustAndWorkerKeyOnGateway(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "rlark-ssh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s := SSH{IdentityFile: "/private/server-key", KnownHosts: "/private/server-hosts", WorkerIdentityFile: "/root/.ssh/worker-key", WorkerKnownHosts: "/root/.ssh/worker-hosts", ControlDirectory: dir}
	if err := s.validateControlDirectory(); err != nil {
		t.Fatal(err)
	}
	args := s.arguments(Target{Gateway: ext.EnrollmentNode{Address: "203.0.113.2", Port: 22}, Node: ext.EnrollmentNode{Role: ext.NodeRoleWorker, Address: "10.0.3.85", Port: 22}}, "probe")
	joined := strings.Join(args, " ")
	for _, required := range []string{"-oControlMaster=auto", "-oControlPersist=60", filepath.Join(dir, "%C"), "-oStrictHostKeyChecking=yes", "-oForwardAgent=no"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("missing %q in SSH arguments", required)
		}
	}
	remote := args[len(args)-1]
	if !strings.Contains(remote, "'/root/.ssh/worker-key'") || !strings.Contains(remote, "'root@10.0.3.85'") || strings.Contains(remote, "ControlPath") {
		t.Fatal("worker SSH is not isolated to the gateway's own identity")
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := s.validateControlDirectory(); err == nil {
		t.Fatal("public control directory accepted")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	s.ControlDirectory = link
	if err := s.validateControlDirectory(); err == nil {
		t.Fatal("symlink control directory accepted")
	}
	s.ControlDirectory = ""
	if err := s.validateControlDirectory(); err != nil {
		t.Fatal("legacy SSH profile rejected")
	}
	s.ControlDirectory = filepath.Join(dir, strings.Repeat("x", 90))
	if err := os.Mkdir(s.ControlDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.validateControlDirectory(); err == nil {
		t.Fatal("oversized control socket path accepted")
	}
}
