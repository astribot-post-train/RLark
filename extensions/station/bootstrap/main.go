// station-bootstrap turns a machine into a node. It is the entry point of the
// installer image an operator runs once, privileged and in the host's PID and
// network namespaces:
//
//	docker run --rm --privileged --net=host --pid=host -v CA:/ca.crt:ro \
//	  -e RLARK_SERVER=https://station-api:18443 -e RLARK_CODE=<one-time code> rlark/node-bootstrap
//
// It copies its bundle to the host, creates the machine's WireGuard key,
// redeems the code at Station API for the tunnel configuration and a join
// token, and hands over to install.sh, which runs as a host process.
package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"sigs.k8s.io/yaml"
)

const (
	hostRoot = "/proc/1/root" // the host's filesystem, visible because of --pid=host
	stage    = "/var/lib/rlark/bootstrap"
)

type answer struct {
	State         string          `json:"state"`
	Reason        string          `json:"reason"`
	ClusterID     string          `json:"cluster_id"`
	NodeName      string          `json:"node_name"`
	TunnelAddress string          `json:"tunnel_address"`
	Config        json.RawMessage `json:"config"`
	Join          struct {
		APIServer  string `json:"api_server"`
		Token      string `json:"token"`
		CACertHash string `json:"ca_cert_hash"`
	} `json:"join"`
	Labels string `json:"labels"`
	Taints string `json:"taints"`
}

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) == 4 && os.Args[1] == "kubelet-overrides" {
		kubeletOverrides(os.Args[2], os.Args[3])
		return
	}
	if os.Getenv("PHASE") != "" {
		upgrade()
		return
	}
	server, code := os.Getenv("RLARK_SERVER"), os.Getenv("RLARK_CODE")
	ca, err := os.ReadFile("/ca.crt")
	if err != nil && os.Getenv("RLARK_CA") != "" {
		// A Job that moves a node hands the CA over in the environment.
		ca, err = []byte(os.Getenv("RLARK_CA")), nil
	}
	if server == "" || code == "" || err != nil {
		log.Fatal("RLARK_SERVER, RLARK_CODE and the CA (/ca.crt or RLARK_CA) are required")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		log.Fatal("/ca.crt holds no certificate")
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}

	// A wrong clock fails later in ways that do not name it: certificates
	// that are "not yet valid", a join token that has "expired".
	response, err := client.Get(server + "/healthz")
	if err != nil {
		log.Fatalf("Station API is not reachable at %s: %v", server, err)
	}
	response.Body.Close()
	if at, err := http.ParseTime(response.Header.Get("Date")); err == nil {
		if skew := time.Since(at); skew > time.Minute || skew < -time.Minute {
			log.Fatalf("this machine's clock is %s off the server's; set the clock and run again", skew.Round(time.Second))
		}
	}

	log.Print("copying the bundle to the host")
	if err := copyTree("/bundle", hostRoot+stage); err != nil {
		log.Fatalf("copy the bundle: %v", err)
	}
	if err := os.WriteFile(hostRoot+stage+"/ca.crt", ca, 0644); err != nil {
		log.Fatal(err)
	}
	if err := syscall.Chroot(hostRoot); err != nil {
		log.Fatalf("enter the host filesystem (the container must be privileged, with --pid=host): %v", err)
	}
	if err := os.Chdir(stage); err != nil {
		log.Fatal(err)
	}

	key, err := exec.Command(stage+"/bin/station-gateway", "--public-key").Output()
	if err != nil {
		log.Fatalf("create the WireGuard key: %v", err)
	}
	request, _ := json.Marshal(map[string]string{"code": code, "public_key": strings.TrimSpace(string(key))})
	var granted answer
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(2 * time.Second) {
		response, err := client.Post(server+"/v1/bootstrap/redeem", "application/json", bytes.NewReader(request))
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			response.Body.Close()
			_ = json.Unmarshal(body, &granted)
			if granted.State == "ready" {
				break
			}
			if response.StatusCode != http.StatusAccepted {
				log.Fatalf("the code was refused (%d): %s", response.StatusCode, body)
			}
		}
		if time.Now().After(deadline) {
			log.Fatalf("no tunnel configuration after two minutes: %s %v", granted.Reason, err)
		}
	}
	log.Printf("admitted as %s in %s, tunnel address %s", granted.NodeName, granted.ClusterID, granted.TunnelAddress)

	var tunnel struct {
		Interface string `json:"interface"`
		Peers     []struct {
			AllowedIPs []string `json:"allowedIPs"`
		} `json:"peers"`
	}
	if err := json.Unmarshal(granted.Config, &tunnel); err != nil || tunnel.Interface == "" || len(tunnel.Peers) == 0 {
		log.Fatalf("the tunnel configuration names no interface or no peer: %v", err)
	}
	if err := os.MkdirAll("/etc/rlark/station", 0755); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile("/etc/rlark/station/bootstrap.json", granted.Config, 0644); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile("/etc/rlark/station/ca.crt", ca, 0644); err != nil {
		log.Fatal(err)
	}
	settings := append(os.Environ(),
		"PATH=/opt/rlark/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"NODE_NAME="+granted.NodeName, "TUNNEL_ADDRESS="+granted.TunnelAddress, "TUNNEL_INTERFACE="+tunnel.Interface,
		"JOIN_API_SERVER="+granted.Join.APIServer, "JOIN_TOKEN="+granted.Join.Token, "JOIN_CA_HASH="+granted.Join.CACertHash,
		"NODE_LABELS="+granted.Labels, "NODE_TAINTS="+granted.Taints)
	if os.Getenv("RLARK_MIGRATE") != "" {
		migrate(settings)
		return
	}
	install := hostCommand(stage + "/install.sh")
	install.Stdout, install.Stderr = os.Stdout, os.Stderr
	install.Env = settings
	if err := install.Run(); err != nil {
		log.Fatalf("install: %v", err)
	}
}

// migrate runs install.sh as a unit of the host's systemd: the machine is a
// node already and this program runs in one of its pods, which the node's
// leaving its cluster ends. The unit goes on; its output is install.log.
func migrate(settings []string) {
	arguments := []string{"--unit", "rlark-node-migrate", "--collect", "--quiet"}
	for _, setting := range settings {
		if name := strings.SplitN(setting, "=", 2)[0]; strings.HasPrefix(name, "RLARK_") || strings.HasPrefix(name, "NODE_") || strings.HasPrefix(name, "TUNNEL_") || strings.HasPrefix(name, "JOIN_") || name == "PATH" {
			arguments = append(arguments, "--setenv="+setting)
		}
	}
	arguments = append(arguments, "/bin/sh", "-c", "exec /bin/sh "+stage+"/install.sh > "+stage+"/install.log 2>&1")
	if output, err := exec.Command("systemd-run", arguments...).CombinedOutput(); err != nil {
		log.Fatalf("start the migration unit: %v: %s", err, output)
	}
	log.Printf("migration runs as rlark-node-migrate.service; its log is %s/install.log", stage)
	for shown := 0; ; time.Sleep(time.Second) {
		if data, err := os.ReadFile(stage + "/install.log"); err == nil && len(data) > shown {
			os.Stdout.Write(data[shown:])
			shown = len(data)
		}
		if state, _ := exec.Command("systemctl", "is-active", "rlark-node-migrate").Output(); strings.TrimSpace(string(state)) != "active" && strings.TrimSpace(string(state)) != "activating" {
			return
		}
	}
}

func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(to, strings.TrimPrefix(path, from))
		if entry.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		source, err := os.Open(path)
		if err != nil {
			return err
		}
		defer source.Close()
		temporary := target + ".new"
		destination, err := os.OpenFile(temporary, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(destination, source); err != nil {
			destination.Close()
			return err
		}
		if err := destination.Close(); err != nil {
			return err
		}
		if err := os.Rename(temporary, target); err != nil {
			return fmt.Errorf("replace %s: %w", target, err)
		}
		return nil
	})
}

// upgrade is the other use of the image: a Job pinned to a node runs it to
// take that node's Kubernetes one minor version up (upgrade.sh).
func upgrade() {
	if err := copyTree("/bundle", hostRoot+stage); err != nil {
		log.Fatalf("copy the bundle: %v", err)
	}
	// upgrade.sh calls this program again on the host for kubelet-overrides.
	self, err := os.ReadFile("/station-bootstrap")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(hostRoot+stage+"/station-bootstrap", self, 0755); err != nil {
		log.Fatal(err)
	}
	if err := syscall.Chroot(hostRoot); err != nil {
		log.Fatalf("enter the host filesystem (the container must be privileged, with hostPID): %v", err)
	}
	if err := os.Chdir(stage); err != nil {
		log.Fatal(err)
	}
	run := hostCommand(stage + "/upgrade.sh")
	run.Stdout, run.Stderr = os.Stdout, os.Stderr
	run.Env = append(os.Environ(), "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	if err := run.Run(); err != nil {
		log.Fatalf("upgrade: %v", err)
	}
}

// kubeletOverrides prints, as a kubeadm patch of the KubeletConfiguration,
// the settings in which this node's kubelet configuration differs from the
// cluster's. kubeadm writes the node's file anew from the cluster's on every
// upgrade; the patch carries what was set on this node only (swap handling on
// a workstation, CPU reservation on a robot) across it.
func kubeletOverrides(nodeFile, clusterFile string) {
	read := func(path string) map[string]interface{} {
		data, err := os.ReadFile(path)
		if err != nil {
			log.Fatal(err)
		}
		var config map[string]interface{}
		if err := yaml.Unmarshal(data, &config); err != nil {
			log.Fatalf("%s: %v", path, err)
		}
		return config
	}
	node, cluster := read(nodeFile), read(clusterFile)
	overrides := map[string]interface{}{}
	for key, value := range node {
		if key != "apiVersion" && key != "kind" && !reflect.DeepEqual(value, cluster[key]) {
			overrides[key] = value
		}
	}
	if len(overrides) == 0 {
		return
	}
	overrides["apiVersion"], overrides["kind"] = "kubelet.config.k8s.io/v1beta1", "KubeletConfiguration"
	patch, err := yaml.Marshal(overrides)
	if err != nil {
		log.Fatal(err)
	}
	os.Stdout.Write(patch)
}

// hostCommand runs a script of the bundle as a process of the host. After the
// chroot this program sees the host's files but is still in its container's
// namespaces; kubeadm reads the mount table to find the cgroup hierarchy, so
// the script is started through the host's nsenter in the namespaces of PID 1.
func hostCommand(script string) *exec.Cmd {
	for _, nsenter := range []string{"/usr/bin/nsenter", "/bin/nsenter", "/usr/sbin/nsenter", "/sbin/nsenter"} {
		if _, err := os.Stat(nsenter); err == nil {
			return exec.Command(nsenter, "-t", "1", "-m", "-u", "-i", "-n", "-p", "--", "/bin/sh", script)
		}
	}
	log.Print("the host has no nsenter; running in the container's namespaces")
	return exec.Command("/bin/sh", script)
}
