package provisioning

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"time"
)

// BundleSource streams the installer and its expected SHA-256.
type BundleSource interface {
	Open(context.Context, string) (io.ReadCloser, int64, error)
	BundleDigest(context.Context) (string, error)
}

// InstalledBundle is what SendInstaller leaves on a machine: Station API's installer, of sha256, and the machine's
// tunnel key, made there; registryCA is the CA of the site registry the installer trusts.
type InstalledBundle struct {
	SHA256, PublicKey, RegistryCA string
}

// SendInstaller gives the machine Station API's installer, unless the one it holds (of sha256 have) is that one
// already, and has the machine make its tunnel key, which never leaves it. While it streams, progress is told how
// much has gone every 5 s.
func SendInstaller(ctx context.Context, bootstrap BundleSource, machine Machine, registry, have string, progress func(string)) (InstalledBundle, string, error) {
	var installed InstalledBundle
	want, err := bootstrap.BundleDigest(ctx)
	if err != nil {
		return installed, "", fmt.Errorf("read the installer's sha256 from Station API: %w", err)
	}
	var out, readback string
	if have == want {
		if out, err = machine.Run(ctx, TunnelKeyScript(registry), nil, nil); err != nil {
			return installed, "", err
		}
		readback = fmt.Sprintf("the installer of sha256 %s… is on the machine already, not sent again", ShortDigest(want))
	} else {
		installer, size, err := bootstrap.Open(ctx, "/v1/bootstrap/bundle")
		if err != nil {
			return installed, "", fmt.Errorf("take the installer from Station API: %w", err)
		}
		defer func() { _ = installer.Close() }()
		sent := &sentCounter{reader: installer, size: size, progress: progress, started: time.Now()}
		if out, err = machine.Run(ctx, BundleScript(registry, want), sent, nil); err != nil {
			return installed, "", err
		}
		readback = fmt.Sprintf("installer sha256 %s… sent (%s in %s)", ShortDigest(OutputValues(out)["sha256"]), megabytes(sent.count), time.Since(sent.started).Round(time.Second))
	}
	values := OutputValues(out)
	ca, _ := base64.StdEncoding.DecodeString(values["ca"])
	installed = InstalledBundle{SHA256: want, PublicKey: values["publickey"], RegistryCA: string(ca)}
	readback += "; tunnel public key " + installed.PublicKey
	if values["sha256"] != "" && values["sha256"] != want {
		return installed, readback, fmt.Errorf("the machine holds an installer of sha256 %s, Station API's is %s", values["sha256"], want)
	}
	if !strings.Contains(installed.RegistryCA, "BEGIN CERTIFICATE") {
		return installed, readback, fmt.Errorf("the installer holds no CA of %s (certs.d/%s/ca.crt)", registry, registry)
	}
	if len(installed.PublicKey) != 44 {
		return installed, readback, fmt.Errorf("station-gateway answered no WireGuard public key (%q)", installed.PublicKey)
	}
	return installed, readback, nil
}

// sentCounter counts what the installer's stream has given, and tells progress every 5 s.
type sentCounter struct {
	reader   io.Reader
	size     int64
	count    int64
	progress func(string)
	started  time.Time
	told     time.Time
}

func (s *sentCounter) Read(p []byte) (int, error) {
	n, err := s.reader.Read(p)
	s.count += int64(n)
	if now := time.Now(); now.Sub(s.told) >= 5*time.Second {
		s.told = now
		rate := float64(s.count) / max(now.Sub(s.started).Seconds(), 1) / (1 << 20)
		s.progress(fmt.Sprintf("sent %s of %s, %.1f MB/s", megabytes(s.count), megabytes(s.size), rate))
	}
	return n, err
}

func megabytes(n int64) string {
	if n >= 1<<30 {
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	}
	return fmt.Sprintf("%d MB", n>>20)
}

// ShortDigest abbreviates a digest for progress readbacks.
func ShortDigest(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}
