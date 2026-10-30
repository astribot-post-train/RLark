package provision

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/agent"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/controller"
	"github.com/rlinf/rlark/apps/rlark/pkg/auth/cert"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func packageRequest(t *testing.T, id NodeIdentity) PackageRequest {
	t.Helper()
	dir := t.TempDir()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	must(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "dedicated Agent test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	must(t, err)
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	must(t, err)
	ca, keyPath := filepath.Join(dir, "issuer.crt"), filepath.Join(dir, "issuer.key")
	must(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600))
	must(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), 0600))
	binary, err := os.Executable()
	must(t, err)
	return PackageRequest{Node: id, Endpoint: "https://container-api.example.internal:9444", AgentCA: ca, AgentCAKey: keyPath, ServerCA: ca, AgentBinary: binary, SupervisorBinary: binary, Version: "preview-v1", Architecture: runtime.GOARCH, ValidityHours: 24, OutputDirectory: filepath.Join(dir, "package"), InstallDirectory: "/opt/rlark-container", StateDirectory: "/var/lib/rlark-supervisor", Executables: []string{"/usr/bin/python3"}, WorkDirectories: []string{"/workspace/train"}, LogBytes: 1048576}
}

func TestIssuedCertificateUsesRealMTLSAndIsRevokedByCredentialChange(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux ELF packaging")
	}
	s, cn := registered(t)
	req := packageRequest(t, nodeID(cn))
	ctx := context.Background()
	_, err := (controller.ContainerClusterReconciler{Client: s.Client}).Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Namespace: cn.Namespace, Name: cn.Spec.Cluster.Name}})
	must(t, err)
	_, err = (controller.ContainerNodeReconciler{Client: s.Client}).Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cn)})
	must(t, err)
	_, err = s.ExportPackage(ctx, req)
	must(t, err)
	data, err := os.ReadFile(req.AgentCA)
	must(t, err)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		t.Fatal("missing test CA")
	}
	server := httptest.NewUnstartedServer(agent.Handler{Reporter: agent.Reporter{Client: s.Client}})
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	server.StartTLS()
	defer server.Close()
	pair, err := tls.LoadX509KeyPair(filepath.Join(req.OutputDirectory, "agent.crt"), filepath.Join(req.OutputDirectory, "agent.key"))
	must(t, err)
	server.Client().Transport.(*http.Transport).TLSClientConfig.Certificates = []tls.Certificate{pair}
	report := func(want int) {
		t.Helper()
		body := `{"version":"preview-v1","privateAddress":"10.0.3.85","visibleGPUUUIDs":["GPU-a"],"busy":false}`
		response, err := server.Client().Post(server.URL+"/v1/heartbeat", "application/json", strings.NewReader(body))
		must(t, err)
		must(t, response.Body.Close())
		if response.StatusCode != want {
			t.Fatalf("HTTP %d, want %d", response.StatusCode, want)
		}
	}
	report(http.StatusNoContent)
	must(t, s.Client.Get(ctx, client.ObjectKeyFromObject(cn), cn))
	cn.Annotations[agent.CredentialAnnotation] = "rotated"
	must(t, s.Client.Update(ctx, cn))
	report(http.StatusForbidden)
}

func TestPackageIsPrivateScopedAndDoesNotCarryIssuer(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux ELF packaging")
	}
	s, cn := registered(t)
	req := packageRequest(t, nodeID(cn))
	manifest, err := s.ExportPackage(context.Background(), req)
	must(t, err)
	var installed PackageManifest
	data, err := os.ReadFile(filepath.Join(req.OutputDirectory, "manifest.json"))
	must(t, err)
	must(t, json.Unmarshal(data, &installed))
	if !reflect.DeepEqual(*manifest, installed) {
		t.Fatal("manifest mismatch")
	}
	info, err := os.Stat(req.OutputDirectory)
	must(t, err)
	if info.Mode().Perm() != 0700 {
		t.Fatal("package is not private")
	}
	entries, err := os.ReadDir(req.OutputDirectory)
	must(t, err)
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "issuer") || entry.Name() == "INCOMPLETE" || strings.Contains(entry.Name(), "kubeconfig") {
			t.Fatalf("unexpected package file: %s", entry.Name())
		}
		st, err := entry.Info()
		must(t, err)
		if st.Mode().Perm()&0077 != 0 {
			t.Fatal("package contains public files")
		}
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(req.OutputDirectory, "agent.crt"), filepath.Join(req.OutputDirectory, "agent.key"))
	must(t, err)
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	must(t, err)
	m, ok := cert.GetX509CertMeta(leaf)
	if !ok || m["container-node-uid"] != string(cn.UID) || m["container-tenant"] != req.Node.TenantID || len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth || leaf.IsCA || !leaf.NotAfter.Equal(manifest.NotAfter) {
		t.Fatalf("wrong certificate scope: %+v", m)
	}
	for _, name := range []string{"start-agent", "start-supervisor"} {
		out, err := exec.Command("sh", "-n", filepath.Join(req.OutputDirectory, name)).CombinedOutput()
		if err != nil {
			t.Fatalf("launcher syntax: %s %v", out, err)
		}
	}
	if _, err := s.ExportPackage(context.Background(), req); err == nil {
		t.Fatal("existing package overwritten")
	}
}

func TestPackageRejectsInvalidInputsBeforeWriting(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux ELF packaging")
	}
	for _, mode := range []string{"expired leaf", "long validity", "issuer expiry", "public key", "symlink key", "invalid CA", "server CA contains key", "wrong architecture", "script artifact", "relocatable artifact", "overlap state", "work in state", "executable in install", "relative output", "wildcard endpoint", "install newline", "install specifier", "credential", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			s, cn := registered(t)
			req := packageRequest(t, nodeID(cn))
			ctx := context.Background()
			switch mode {
			case "expired leaf":
				req.ValidityHours = 0
			case "long validity":
				req.ValidityHours = 721
			case "issuer expiry":
				req.ValidityHours = 49
			case "public key":
				must(t, os.Chmod(req.AgentCAKey, 0644))
			case "symlink key":
				link := req.AgentCAKey + ".link"
				must(t, os.Symlink(req.AgentCAKey, link))
				req.AgentCAKey = link
			case "invalid CA":
				req.AgentCA = req.AgentCAKey
			case "server CA contains key":
				ca, err := os.ReadFile(req.AgentCA)
				must(t, err)
				key, err := os.ReadFile(req.AgentCAKey)
				must(t, err)
				req.ServerCA = filepath.Join(filepath.Dir(req.AgentCA), "combined.pem")
				must(t, os.WriteFile(req.ServerCA, append(ca, key...), 0600))
			case "wrong architecture":
				if req.Architecture == "amd64" {
					req.Architecture = "arm64"
				} else {
					req.Architecture = "amd64"
				}
			case "script artifact":
				req.AgentBinary = req.AgentCA
			case "relocatable artifact":
				data, err := os.ReadFile(req.AgentBinary)
				must(t, err)
				// ELF e_type at byte 16: retain the matching machine, but
				// identify this as a relocatable object, not an executable.
				data[16], data[17] = 1, 0
				req.AgentBinary = filepath.Join(filepath.Dir(req.AgentCA), "object.elf")
				must(t, os.WriteFile(req.AgentBinary, data, 0700))
			case "overlap state":
				req.StateDirectory = req.InstallDirectory + "/state"
			case "work in state":
				req.WorkDirectories = []string{req.StateDirectory + "/train"}
			case "executable in install":
				req.Executables = []string{req.InstallDirectory + "/python"}
			case "relative output":
				req.OutputDirectory = "relative"
			case "wildcard endpoint":
				req.Endpoint = "http://insecure.internal"
			case "install newline":
				req.InstallDirectory = "/opt/a\ncommand"
			case "install specifier":
				req.InstallDirectory = "/opt/%n"
			case "credential":
				req.Node.CredentialID = "stale"
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if _, err := s.ExportPackage(ctx, req); err == nil {
				t.Fatal("invalid request accepted")
			}
			if _, err := os.Lstat(req.OutputDirectory); !os.IsNotExist(err) {
				t.Fatalf("failed validation wrote package: %v", err)
			}
		})
	}
}

func TestExportRejectsIdentityAndConfigurationChangesDuringExport(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux ELF packaging")
	}
	for _, stage := range []int{2, 3} {
		for _, field := range []string{"credential", "environment"} {
			t.Run(fmt.Sprintf("read-%d-%s", stage, field), func(t *testing.T) {
				s, cn := registered(t)
				req := packageRequest(t, nodeID(cn))
				reads := 0
				s.Client = interceptor.NewClient(s.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, key, obj, opts...); err != nil {
						return err
					}
					if node, ok := obj.(*ext.ContainerNode); ok {
						reads++
						if reads == stage {
							if field == "credential" {
								node.Annotations[agent.CredentialAnnotation] = "rotated"
							} else {
								node.Spec.Environment = "new-environment"
							}
							return c.Update(ctx, node)
						}
					}
					return nil
				}})
				if _, err := s.ExportPackage(context.Background(), req); !errors.Is(err, ErrConflict) {
					t.Fatalf("changed authorization/configuration accepted: %v", err)
				}
				if stage == 2 {
					if _, err := os.Stat(req.OutputDirectory); !os.IsNotExist(err) {
						t.Fatalf("early failure wrote output: %v", err)
					}
				} else if _, err := os.Stat(filepath.Join(req.OutputDirectory, "INCOMPLETE")); err != nil {
					t.Fatalf("failed export lost incomplete marker: %v", err)
				}
			})
		}
	}
}

func TestPackageLauncherChecksArtifactsBeforeExecution(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux ELF packaging")
	}
	s, cn := registered(t)
	req := packageRequest(t, nodeID(cn))
	req.InstallDirectory = req.OutputDirectory
	_, err := s.ExportPackage(context.Background(), req)
	must(t, err)
	check := exec.Command("sha256sum", "-c", "SHA256SUMS")
	check.Dir = req.OutputDirectory
	if out, err := check.CombinedOutput(); err != nil {
		t.Fatalf("export checksum verification failed: %s %v", out, err)
	}
	// A modified artifact must prevent launching even if its filename is valid.
	must(t, os.WriteFile(filepath.Join(req.OutputDirectory, "container-runtime-agent"), []byte("#!/bin/sh\necho WORKLOAD_STARTED\n"), 0700))
	// Simulate the root check to exercise checksum rejection on non-root CI too.
	probeDir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(probeDir, "id"), []byte("#!/bin/sh\necho 0\n"), 0700))
	launcher := exec.Command("sh", filepath.Join(req.OutputDirectory, "start-agent"))
	launcher.Env = append(os.Environ(), "PATH="+probeDir+":"+os.Getenv("PATH"), "LC_ALL=C")
	out, err := launcher.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "FAILED") || strings.Contains(string(out), "WORKLOAD_STARTED") {
		t.Fatalf("modified artifact executed: %s %v", out, err)
	}
}

func TestLauncherArgumentsPreserveLiteralShellCharacters(t *testing.T) {
	value := "/opt/path with 'quotes' and $(false); `false` $HOME"
	out, err := exec.Command("sh", "-c", command("printf", "%s", value)).CombinedOutput()
	must(t, err)
	if string(out) != value {
		t.Fatalf("argument was interpreted: %q", out)
	}
}
