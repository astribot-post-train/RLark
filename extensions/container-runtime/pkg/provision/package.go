package provision

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/agent"
	dockerruntime "github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/runtime/docker"
	"github.com/rlinf/rlark/apps/rlark/pkg/auth/cert"
)

// PackageRequest contains local issuer inputs and explicit target paths. CAKey
// and kubeconfig are provisioning-machine inputs and never enter the package.
type PackageRequest struct {
	Node                    NodeIdentity `json:"node"`
	Endpoint                string       `json:"endpoint"`
	AgentCA                 string       `json:"agentCA"`
	AgentCAKey              string       `json:"agentCAKey"`
	ServerCA                string       `json:"serverCA"`
	AgentBinary             string       `json:"agentBinary"`
	SupervisorBinary        string       `json:"supervisorBinary"`
	Runtime                 ext.Runtime  `json:"runtime,omitempty"`
	DockerHost              string       `json:"dockerHost,omitempty"`
	Version                 string       `json:"version"`
	Architecture            string       `json:"architecture"`
	ValidityHours           int          `json:"validityHours"`
	OutputDirectory         string       `json:"outputDirectory"`
	InstallDirectory        string       `json:"installDirectory"`
	StateDirectory          string       `json:"stateDirectory"`
	Executables             []string     `json:"executables"`
	WorkDirectories         []string     `json:"workDirectories"`
	LogBytes                int64        `json:"logBytes"`
	EnableExecution         bool         `json:"enableExecution"`
	EnableLogReads          bool         `json:"enableLogReads"`
	EnableDockerDiagnostics bool         `json:"enableDockerDiagnostics"`
	EnableUpgrades          bool         `json:"enableUpgrades"`
}

// PackageManifest records public identity, validity and artifact integrity.
type PackageManifest struct {
	Node         NodeIdentity      `json:"node"`
	ClusterUID   string            `json:"clusterUID"`
	InstanceID   string            `json:"instanceID"`
	Endpoint     string            `json:"endpoint"`
	Version      string            `json:"version"`
	Architecture string            `json:"architecture"`
	NotAfter     time.Time         `json:"notAfter"`
	SHA256       map[string]string `json:"sha256"`
}

func targetPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" && !strings.ContainsAny(path, "\x00\r\n%")
}
func within(path, parent string) bool { return path == parent || strings.HasPrefix(path, parent+"/") }
func quote(value string) string       { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
func command(argv ...string) string {
	var parts []string
	for _, v := range argv {
		parts = append(parts, quote(v))
	}
	return "exec " + strings.Join(parts, " ") + "\n"
}
func readPrivateKey(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("issuer key must be a private regular file")
	}
	return nil
}
func issuer(req PackageRequest, now time.Time) (tls.Certificate, *x509.Certificate, error) {
	if err := readPrivateKey(req.AgentCAKey); err != nil {
		return tls.Certificate{}, nil, err
	}
	pair, err := tls.LoadX509KeyPair(req.AgentCA, req.AgentCAKey)
	if err != nil {
		return pair, nil, fmt.Errorf("load Agent issuer: %w", err)
	}
	ca, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return pair, nil, err
	}
	if !ca.IsCA || !ca.BasicConstraintsValid || ca.KeyUsage&x509.KeyUsageCertSign == 0 || now.Before(ca.NotBefore) || !now.Before(ca.NotAfter) {
		return pair, nil, errors.New("current signing CA certificate required")
	}
	if req.ValidityHours < 1 || req.ValidityHours > 30*24 || now.Add(time.Duration(req.ValidityHours)*time.Hour).After(ca.NotAfter) {
		return pair, nil, errors.New("certificate validity must be 1..720 hours within issuer validity")
	}
	return pair, ca, nil
}

func issue(req PackageRequest, id agent.Identity, now time.Time) ([]byte, []byte, time.Time, error) {
	pair, ca, err := issuer(req, now)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	before := now.Add(-time.Minute)
	if before.Before(ca.NotBefore) {
		before = ca.NotBefore
	}
	leaf := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "container-agent:" + id.UID}, NotBefore: before, NotAfter: now.Add(time.Duration(req.ValidityHours) * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	cert.SetX509CertMeta(leaf, id.Metadata())
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, pub, pair.PrivateKey)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), leaf.NotAfter, nil
}

func readArtifact(path, architecture string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 256<<20 {
		return nil, errors.New("bounded regular Linux executable required")
	}
	f, err := elf.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read Linux ELF artifact: %w", err)
	}
	machine := f.Machine
	kind, class, abi := f.Type, f.Class, f.OSABI
	closeErr := f.Close()
	if closeErr != nil {
		return nil, closeErr
	}
	if architecture == "amd64" && machine != elf.EM_X86_64 || architecture == "arm64" && machine != elf.EM_AARCH64 {
		return nil, errors.New("artifact architecture mismatch")
	}
	if kind != elf.ET_EXEC && kind != elf.ET_DYN || class != elf.ELFCLASS64 || abi != elf.ELFOSABI_NONE && abi != elf.ELFOSABI_LINUX {
		return nil, errors.New("64-bit Linux executable artifact required")
	}
	return os.ReadFile(path)
}

// ExportPackage writes a new private directory exclusively. A failed write
// leaves an INCOMPLETE marker; it never overwrites or deletes an existing path.
// This is an offline administrator export, not automatic enrollment/renewal.
func (s Service) ExportPackage(ctx context.Context, req PackageRequest) (*PackageManifest, error) {
	if req.Runtime == "" {
		req.Runtime = "Raw"
	}
	if req.EnableLogReads && !req.EnableExecution {
		return nil, errors.New("log reads require scoped task execution")
	}
	if req.Architecture != "amd64" && req.Architecture != "arm64" {
		return nil, errors.New("linux amd64 or arm64 architecture required")
	}
	if strings.TrimSpace(req.Version) == "" || len(req.Version) > 128 || strings.ContainsAny(req.Version, "\x00\r\n") {
		return nil, errors.New("explicit bounded Agent version required")
	}
	if !targetPath(req.InstallDirectory) {
		return nil, errors.New("separate canonical install/state paths, workload paths and positive log bound required")
	}
	switch req.Runtime {
	case "Raw":
		if req.EnableDockerDiagnostics {
			return nil, errors.New("docker diagnostics require Docker runtime")
		}
		if req.DockerHost != "" {
			return nil, errors.New("raw package does not accept dockerHost")
		}
		if !targetPath(req.StateDirectory) || within(req.InstallDirectory, req.StateDirectory) || within(req.StateDirectory, req.InstallDirectory) || req.LogBytes <= 0 || len(req.Executables) == 0 || len(req.WorkDirectories) == 0 || req.SupervisorBinary == "" {
			return nil, errors.New("raw package requires supervisor, state, workload paths and log bound")
		}
	case "Docker":
		if req.EnableLogReads || req.EnableUpgrades || req.SupervisorBinary != "" || len(req.Executables) != 0 || len(req.WorkDirectories) != 0 || req.LogBytes != 0 {
			return nil, errors.New("docker package cannot include Raw supervisor, log or upgrade options")
		}
		if req.EnableDockerDiagnostics && !req.EnableExecution {
			return nil, errors.New("docker diagnostics require task execution")
		}
		if req.EnableExecution && (!targetPath(req.StateDirectory) || within(req.InstallDirectory, req.StateDirectory) || within(req.StateDirectory, req.InstallDirectory)) {
			return nil, errors.New("docker execution requires a private state directory")
		}
		if !req.EnableExecution && req.StateDirectory != "" {
			return nil, errors.New("docker state directory requires task execution")
		}
		if req.DockerHost == "" {
			req.DockerHost = dockerruntime.DefaultLocalHost
		}
		if err := dockerruntime.ValidateLocalHost(req.DockerHost); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("runtime must be Raw or Docker")
	}
	if req.EnableUpgrades && (within(req.StateDirectory+"-agent-upgrades", req.InstallDirectory) || within(req.InstallDirectory, req.StateDirectory+"-agent-upgrades")) {
		return nil, errors.New("updater state must be separate from install")
	}
	for _, path := range append(append([]string{}, req.Executables...), req.WorkDirectories...) {
		if req.EnableUpgrades && (within(req.StateDirectory+"-agent-upgrades", path) || within(path, req.StateDirectory+"-agent-upgrades")) {
			return nil, errors.New("workload paths must exclude updater state")
		}
		if !targetPath(path) || req.Runtime == "Raw" && (within(req.StateDirectory, path) || within(path, req.StateDirectory)) || within(req.InstallDirectory, path) || within(path, req.InstallDirectory) {
			return nil, errors.New("canonical workload paths must exclude private install/state paths")
		}
	}
	if !targetPath(req.OutputDirectory) {
		return nil, errors.New("absolute canonical output directory required")
	}
	node, err := s.ReadNode(ctx, req.Node)
	if err != nil {
		return nil, err
	}
	if node.Spec.Runtime != req.Runtime {
		return nil, ErrConflict
	}
	id := agent.Identity{Namespace: node.Namespace, Name: node.Name, UID: string(node.UID), ClusterUID: node.Spec.Cluster.UID, InstanceID: node.Spec.InstanceID, TenantID: req.Node.TenantID, CredentialID: req.Node.CredentialID}
	certificate, key, expires, err := issue(req, id, time.Now().UTC().Truncate(time.Second))
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(req.ServerCA)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	rest := bytes.TrimSpace(ca)
	certificates := 0
	for len(rest) > 0 {
		// Only public CA certificates may cross this boundary. CertPool's
		// permissive PEM parser also accepts bundles containing private keys.
		if !bytes.HasPrefix(rest, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, errors.New("API server CA must contain only public CA certificates")
		}
		block, tail := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, errors.New("invalid API server CA certificate bundle")
		}
		public, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse API server CA: %w", err)
		}
		if !public.IsCA || !public.BasicConstraintsValid {
			return nil, errors.New("API server trust bundle requires CA certificates")
		}
		pool.AddCert(public)
		certificates++
		rest = bytes.TrimSpace(tail)
	}
	if certificates == 0 {
		return nil, errors.New("API server CA contains no certificate")
	}
	pair, err := tls.X509KeyPair(certificate, key)
	if err != nil {
		return nil, err
	}
	transport, err := agent.NewClient(req.Endpoint, pair, pool)
	if err != nil {
		return nil, err
	}
	transport.HTTPClient.CloseIdleConnections()
	agentBinary, err := readArtifact(req.AgentBinary, req.Architecture)
	if err != nil {
		return nil, err
	}
	var supervisorBinary []byte
	if req.Runtime == "Raw" {
		supervisorBinary, err = readArtifact(req.SupervisorBinary, req.Architecture)
		if err != nil {
			return nil, err
		}
	}
	base := req.InstallDirectory
	files := map[string][]byte{"agent.crt": certificate, "agent.key": key, "server-ca.crt": ca, "container-runtime-agent": agentBinary}
	argv := []string{base + "/container-runtime-agent", "--runtime=" + strings.ToLower(string(req.Runtime)), "--endpoint=" + req.Endpoint, "--tls-cert=" + base + "/agent.crt", "--tls-key=" + base + "/agent.key", "--server-ca=" + base + "/server-ca.crt", "--version=" + req.Version, "--private-address=" + node.Spec.PrivateAddress}
	if req.Runtime == "Raw" {
		files["raw-supervisor"] = supervisorBinary
		argv = append(argv, "--supervisor-socket="+req.StateDirectory+"/supervisor.sock")
	} else {
		argv = append(argv, "--docker-host="+req.DockerHost)
		if req.EnableExecution {
			argv = append(argv, "--state-directory="+req.StateDirectory)
		}
	}
	if req.EnableExecution {
		argv = append(argv, "--enable-task-execution=true")
	}
	if req.EnableLogReads {
		argv = append(argv, "--enable-log-reads=true")
	}
	if req.EnableDockerDiagnostics {
		argv = append(argv, "--enable-docker-diagnostics=true")
	}
	if req.EnableUpgrades {
		argv = append(argv, "--enable-agent-upgrades=true", "--agent-install-path="+base+"/container-runtime-agent", "--upgrade-state-directory="+req.StateDirectory+"-agent-upgrades")
	}
	preflight := "#!/bin/sh\nset -eu\n[ \"$(id -u)\" -eq 0 ] || { echo 'root required' >&2; exit 1; }\ncd " + quote(base) + "\n[ ! -e INCOMPLETE ] || { echo 'incomplete package' >&2; exit 1; }\nsha256sum -c SHA256SUMS >&2\n"
	if req.EnableUpgrades {
		// Keep package validation for every immutable file; verify the Agent against
		// its original digest or one of the private updater-persisted switch digests.
		preflight = strings.ReplaceAll(preflight, "sha256sum -c SHA256SUMS >&2\n", "sed '/  container-runtime-agent$/d' SHA256SUMS | sha256sum -c - >&2\n"+
			"(printf '%s  container-runtime-agent\\n' "+quote(fmt.Sprintf("%x", sha256.Sum256(agentBinary)))+" | sha256sum -c - >&2) || sha256sum -c container-runtime-agent.upgrade-target.sha256 >&2 || sha256sum -c container-runtime-agent.upgrade-source.sha256 >&2\n")
	}
	files["start-agent"] = []byte(preflight + command(argv...))
	config := struct {
		Backend          string   `json:"backend"`
		WorkerExecutable string   `json:"workerExecutable"`
		StateDirectory   string   `json:"stateDirectory"`
		Executables      []string `json:"executables"`
		WorkDirectories  []string `json:"workDirectories"`
		LogBytes         int64    `json:"logBytes"`
	}{"process-tree", base + "/raw-supervisor", req.StateDirectory, req.Executables, req.WorkDirectories, req.LogBytes}
	if req.Runtime == "Raw" {
		files["supervisor.json"], err = json.MarshalIndent(config, "", "  ")
		if err != nil {
			return nil, err
		}
		files["start-supervisor"] = []byte(preflight + command(base+"/raw-supervisor", "--config", base+"/supervisor.json"))
	}
	unit := func(desc, script, extra string) []byte {
		return []byte("[Unit]\nDescription=" + desc + "\nAfter=network-online.target\n" + extra + "\n[Service]\nType=simple\nUser=root\nExecStart=" + fmt.Sprintf("%q", strings.ReplaceAll(base+"/"+script, "$", "$$")) + "\nRestart=on-failure\nRestartSec=5\nKillMode=process\n\n[Install]\nWantedBy=multi-user.target\n")
	}
	if req.Runtime == "Raw" {
		files["rlark-container-agent.service"] = unit("RLark container Agent", "start-agent", "After=rlark-raw-supervisor.service\n")
		files["rlark-raw-supervisor.service"] = unit("RLark trusted root Raw supervisor", "start-supervisor", "")
	} else {
		files["rlark-container-agent.service"] = unit("RLark Docker observation Agent", "start-agent", "After=docker.service\n")
	}
	manifest := &PackageManifest{Node: req.Node, ClusterUID: id.ClusterUID, InstanceID: id.InstanceID, Endpoint: req.Endpoint, Version: req.Version, Architecture: req.Architecture, NotAfter: expires, SHA256: map[string]string{}}
	var checks []string
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		digest := sha256.Sum256(files[name])
		manifest.SHA256[name] = hex.EncodeToString(digest[:])
		checks = append(checks, manifest.SHA256[name]+"  "+name)
	}
	files["SHA256SUMS"] = []byte(strings.Join(checks, "\n") + "\n")
	files["manifest.json"], err = json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	// Re-read authorization after local issuance/artifact work; a credential or
	// native replacement during export must not produce a completed package.
	current, err := s.ReadNode(ctx, req.Node)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(current.Spec, node.Spec) {
		return nil, ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.Mkdir(req.OutputDirectory, 0700); err != nil {
		return nil, fmt.Errorf("create exclusive package directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(req.OutputDirectory, "INCOMPLETE"), []byte("package export did not finish\n"), 0600); err != nil {
		return nil, err
	}
	for name, data := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		mode := os.FileMode(0600)
		if name == "container-runtime-agent" || name == "raw-supervisor" || strings.HasPrefix(name, "start-") {
			mode = 0700
		}
		if err := os.WriteFile(filepath.Join(req.OutputDirectory, name), data, mode); err != nil {
			return nil, err
		}
	}
	current, err = s.ReadNode(ctx, req.Node)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(current.Spec, node.Spec) {
		return nil, ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.Remove(filepath.Join(req.OutputDirectory, "INCOMPLETE")); err != nil {
		return nil, err
	}
	return manifest, nil
}
