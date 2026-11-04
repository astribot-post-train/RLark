package enrollment

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	ext "github.com/astribot-post-train/rlark/extensions/container-runtime/core/api/v1alpha1"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/core/pkg/artifact"
	"github.com/astribot-post-train/rlark/extensions/container-runtime/pkg/provision"
	tenant "github.com/rlinf/rlark/api/metadata"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed node_services.py
var managerScript []byte

// Profile is trusted controller-local configuration, never accepted from UI.
// GatewayFiles must contain only gateway/peer leaf credentials and configuration,
// not issuer private keys. Launch values may expand $INSTALL,
// $NODE_PRIVATE_ADDRESS and $GATEWAY_PRIVATE_ADDRESS from verified discovery.
type Profile struct {
	ClusterUID       string            `json:"clusterUID"`
	AgentCA          string            `json:"agentCA"`
	AgentCAKey       string            `json:"agentCAKey"`
	ServerCA         string            `json:"serverCA"`
	Endpoint         string            `json:"endpoint"`
	SupervisorBinary string            `json:"supervisorBinary"`
	GatewayBinary    string            `json:"gatewayBinary"`
	GatewayFiles     map[string]string `json:"gatewayFiles"`
	GatewayArgv      []string          `json:"gatewayArgv"`
	AgentArgs        []string          `json:"agentArgs"`
	InstallDirectory string            `json:"installDirectory"`
	StateDirectory   string            `json:"stateDirectory"`
	Executables      []string          `json:"executables"`
	WorkDirectories  []string          `json:"workDirectories"`
	// Per-node overrides describe differing existing runtime layouts on A/B/C.
	ExecutablesByNode     map[string][]string `json:"executablesByNode,omitempty"`
	WorkDirectoriesByNode map[string][]string `json:"workDirectoriesByNode,omitempty"`
	Python                string              `json:"python"`
	Environment           string              `json:"environment"`
	NetworkScope          string              `json:"networkScope"`
	AllowedGatewayCIDRs   []string            `json:"allowedGatewayCIDRs"`
	AllowedWorkerCIDRs    []string            `json:"allowedWorkerCIDRs"`
	SSH                   SSH                 `json:"ssh"`
}

// Packages reuses the existing scoped certificate/package exporter. No signing
// authority or management kubeconfig is included in a node package.
type Packages struct {
	Client  client.Client
	Store   artifact.Store
	Profile Profile
}

// Bundle builds a private tar stream. It is regenerated safely on retry; remote
// installation compares immutable identity before accepting or using files.
func (p Packages) Bundle(ctx context.Context, node *ext.ContainerNode, release ext.AgentReleaseSpec, gatewayAddress string) ([]byte, string, error) {
	if p.Store == nil {
		return nil, "", errors.New("artifact store required")
	}
	data, err := p.Store.Load(ctx, release)
	if err != nil {
		return nil, "", err
	}
	tmp, err := os.MkdirTemp("", "rlark-enroll-")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(tmp)
	agentPath := filepath.Join(tmp, "agent")
	if err := os.WriteFile(agentPath, data, 0700); err != nil {
		return nil, "", err
	}
	install := filepath.Join(p.Profile.InstallDirectory, string(node.UID), release.SHA256)
	state := filepath.Join(p.Profile.StateDirectory, string(node.UID))
	expand := func(value string) (string, error) {
		return expandLaunchValue(value, install, node.Spec.PrivateAddress, gatewayAddress)
	}
	endpoint, err := expand(p.Profile.Endpoint)
	if err != nil {
		return nil, "", err
	}
	executables, workDirectories, err := p.Profile.workloadPaths(node.Name)
	if err != nil {
		return nil, "", err
	}
	request := provision.PackageRequest{Node: provision.NodeIdentity{Namespace: node.Namespace, Name: node.Name, UID: string(node.UID), TenantID: tenant.IDOf(node), CredentialID: node.Annotations[ext.AgentCredentialAnnotation]}, Runtime: "Raw", Endpoint: endpoint, AgentCA: p.Profile.AgentCA, AgentCAKey: p.Profile.AgentCAKey, ServerCA: p.Profile.ServerCA, AgentBinary: agentPath, SupervisorBinary: p.Profile.SupervisorBinary, Version: release.Version, Architecture: release.Architecture, ValidityHours: 24 * 7, OutputDirectory: filepath.Join(tmp, "package"), InstallDirectory: install, StateDirectory: state + "/tasks", Executables: executables, WorkDirectories: workDirectories, LogBytes: 1 << 20, EnableExecution: true, EnableLogReads: true, EnableUpgrades: true}
	manifest, err := (provision.Service{Client: p.Client}).ExportPackage(ctx, request)
	if err != nil {
		return nil, "", err
	}
	// Node paths contain only API-issued UIDs and validated server configuration.
	files := map[string][]byte{}
	entries, err := os.ReadDir(request.OutputDirectory)
	if err != nil {
		return nil, "", err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return nil, "", errors.New("unexpected package directory")
		}
		b, e := os.ReadFile(filepath.Join(request.OutputDirectory, entry.Name()))
		if e != nil {
			return nil, "", e
		}
		files[entry.Name()] = b
	}
	agentArgv := []string{install + "/start-agent"}
	// Extra arguments are trusted deployment configuration, for example the Ray peer.
	if len(p.Profile.AgentArgs) > 0 {
		script := string(files["start-agent"])
		script = strings.TrimSuffix(script, "\n")
		for _, arg := range p.Profile.AgentArgs {
			value, e := expand(arg)
			if e != nil {
				return nil, "", e
			}
			script += " " + quote(value)
		}
		files["start-agent"] = []byte(script + "\n")
	}
	services := map[string][]string{"agent": agentArgv, "task-manager": {install + "/start-supervisor"}}
	if node.Spec.Role == ext.NodeRoleGateway {
		if p.Profile.GatewayBinary == "" || len(p.Profile.GatewayArgv) == 0 {
			return nil, "", errors.New("Gateway role requires configured management and Ray gateway launcher")
		}
		files["container-runtime-gateway"], err = os.ReadFile(p.Profile.GatewayBinary)
		if err != nil {
			return nil, "", err
		}
		argv := []string{install + "/container-runtime-gateway"}
		for _, arg := range p.Profile.GatewayArgv {
			value, e := expand(arg)
			if e != nil {
				return nil, "", e
			}
			argv = append(argv, value)
		}
		services["gateway"] = argv
	}
	var extraFiles map[string]string
	if node.Spec.Role == ext.NodeRoleGateway {
		extraFiles = p.Profile.GatewayFiles
	}
	for name, path := range extraFiles {
		if filepath.Base(name) != name || name == "." || files[name] != nil {
			return nil, "", errors.New("invalid or conflicting gateway file")
		}
		files[name], err = os.ReadFile(path)
		if err != nil {
			return nil, "", err
		}
	}
	files["node-services.py"] = managerScript
	files["notebook_identity.py"] = []byte(identityScript)
	identity := fmt.Sprintf("%s/%s/%s", node.UID, node.Spec.InstanceID, release.SHA256)
	config := struct {
		Identity           string              `json:"identity"`
		InstanceID         string              `json:"instanceID"`
		PrivateAddress     string              `json:"privateAddress"`
		StateDirectory     string              `json:"stateDirectory"`
		RuntimeDirectories []string            `json:"runtimeDirectories"`
		Services           map[string][]string `json:"services"`
	}{identity, node.Spec.InstanceID, node.Spec.PrivateAddress, state + "/services", []string{state + "/tasks", state + "/tasks-agent-upgrades"}, services}
	files["services.json"], err = json.Marshal(config)
	if err != nil {
		return nil, "", err
	}
	for name, content := range files {
		if name != "manifest.json" && name != "SHA256SUMS" {
			manifest.SHA256[name] = fmt.Sprintf("%x", sha256.Sum256(content))
		}
	}
	files["manifest.json"], err = json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, "", err
	}
	// Bind all immutable launch files and the service configuration.
	names := make([]string, 0, len(files))
	for name := range files {
		if name != "SHA256SUMS" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var sums []string
	for _, name := range names {
		sums = append(sums, fmt.Sprintf("%x  %s", sha256.Sum256(files[name]), name))
	}
	files["SHA256SUMS"] = []byte(strings.Join(sums, "\n") + "\n")
	// Agent can be upgraded by its existing updater; immutable package launcher
	// keeps validating all other files and its source/target digest receipts.
	var result bytes.Buffer
	archive := tar.NewWriter(&result)
	for name, b := range files {
		mode := int64(0600)
		if strings.HasPrefix(name, "start-") || name == "container-runtime-agent" || name == "raw-supervisor" || name == "container-runtime-gateway" {
			mode = 0700
		}
		if err := archive.WriteHeader(&tar.Header{Name: name, Size: int64(len(b)), Mode: mode, ModTime: time.Unix(0, 0)}); err != nil {
			return nil, "", err
		}
		if _, err := archive.Write(b); err != nil {
			return nil, "", err
		}
	}
	if err := archive.Close(); err != nil {
		return nil, "", err
	}
	return result.Bytes(), install, nil
}

// expandLaunchValue uses authenticated discovery, never the SSH/DNAT address.
// Reject unknown placeholders rather than writing an unusable package.
func expandLaunchValue(value, install, nodeAddress, gatewayAddress string) (string, error) {
	for _, address := range []string{nodeAddress, gatewayAddress} {
		ip := net.ParseIP(address)
		if ip == nil || ip.To4() == nil || !ip.IsPrivate() {
			return "", errors.New("verified private IPv4 addresses required for package launch values")
		}
	}
	value = strings.NewReplacer("$INSTALL", install, "$NODE_PRIVATE_ADDRESS", nodeAddress, "$GATEWAY_PRIVATE_ADDRESS", gatewayAddress).Replace(value)
	if strings.Contains(value, "$") {
		return "", errors.New("unknown enrollment launch placeholder")
	}
	return value, nil
}

// installer reads only flat regular files, never follows archive paths/symlinks,
// and never overwrites an existing package or kills an existing service.
const installer = `import fcntl,hashlib,json,os,pathlib,shutil,subprocess,sys,tarfile,tempfile
root=pathlib.Path(sys.argv[1]); identity=sys.argv[2]; python=sys.argv[3]
root.parent.mkdir(mode=0o700,parents=True,exist_ok=True)
for parent in [root.parent,*root.parent.parents]:
 if parent.is_symlink(): raise RuntimeError('symlink install ancestor')
lock=open(str(root)+'.lock','a'); os.chmod(str(root)+'.lock',0o600); fcntl.flock(lock,fcntl.LOCK_EX)
stage=pathlib.Path(tempfile.mkdtemp(prefix='.install-',dir=root.parent))
try:
 archive=tarfile.open(fileobj=sys.stdin.buffer,mode='r|')
 for item in archive:
  if not item.isfile() or pathlib.Path(item.name).name!=item.name or item.size>268435456: raise RuntimeError('invalid package member')
  fd=os.open(stage/item.name,os.O_WRONLY|os.O_CREAT|os.O_EXCL,item.mode & 0o700)
  with os.fdopen(fd,'wb') as output: shutil.copyfileobj(archive.extractfile(item),output)
 config=json.loads((stage/'services.json').read_bytes())
 if config['identity']!=identity: raise RuntimeError('foreign package identity')
 for line in (stage/'SHA256SUMS').read_text().splitlines():
  digest,name=line.split('  ',1)
  if pathlib.Path(name).name!=name or hashlib.sha256((stage/name).read_bytes()).hexdigest()!=digest: raise RuntimeError('package checksum mismatch')
 if root.exists():
  if root.is_symlink(): raise RuntimeError('symlink install root')
  old=json.loads((root/'services.json').read_bytes()); new=dict(config)
  if old!=new: raise RuntimeError('incompatible services')
  for line in (stage/'SHA256SUMS').read_text().splitlines():
   digest,name=line.split('  ',1)
   if name=='container-runtime-agent':
    # A different version requires the explicit idle upgrade path.
    if hashlib.sha256((root/name).read_bytes()).hexdigest()!=digest: raise RuntimeError('agent version changed; use idle upgrade')
   elif name not in ('agent.crt','agent.key','manifest.json','SHA256SUMS') and (root/name).read_bytes()!=(stage/name).read_bytes(): raise RuntimeError('installed package changed')
 else:
  os.rename(stage,root)
finally:
 if stage.exists(): shutil.rmtree(stage)
lock.close()
subprocess.run([python,str(root/'node-services.py'),'ensure',str(root/'services.json')],check=True)
`

func installCommand(path, identity, python string) string {
	return "python3 -c " + quote(installer) + " " + quote(path) + " " + quote(identity) + " " + quote(python)
}

// reader is kept at the call boundary to avoid accidental logging of package bytes.
func packageReader(data []byte) io.Reader { return bytes.NewReader(data) }
