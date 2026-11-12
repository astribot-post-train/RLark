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
	"strings"
	"syscall"
	"time"
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
	server, code := os.Getenv("RLARK_SERVER"), os.Getenv("RLARK_CODE")
	ca, err := os.ReadFile("/ca.crt")
	if server == "" || code == "" || err != nil {
		log.Fatal("RLARK_SERVER, RLARK_CODE and /ca.crt are required")
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
	install := exec.Command("/bin/sh", stage+"/install.sh")
	install.Stdout, install.Stderr = os.Stdout, os.Stderr
	install.Env = append(os.Environ(),
		"PATH=/opt/rlark/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"NODE_NAME="+granted.NodeName, "TUNNEL_ADDRESS="+granted.TunnelAddress, "TUNNEL_INTERFACE="+tunnel.Interface,
		"JOIN_API_SERVER="+granted.Join.APIServer, "JOIN_TOKEN="+granted.Join.Token, "JOIN_CA_HASH="+granted.Join.CACertHash,
		"NODE_LABELS="+granted.Labels, "NODE_TAINTS="+granted.Taints)
	if err := install.Run(); err != nil {
		log.Fatalf("install: %v", err)
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
