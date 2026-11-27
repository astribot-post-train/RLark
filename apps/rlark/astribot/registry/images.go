// Package registry parses, validates and observes OCI registry content.
package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	registryManifestLimit = 4 << 20
	registryConfigLimit   = 4 << 20
)

const (
	mediaTypeOCIIndex        = "application/vnd.oci.image.index.v1+json"
	mediaTypeOCIManifest     = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeDockerList      = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaTypeDockerManifest  = "application/vnd.docker.distribution.manifest.v2+json"
	attestationReferenceType = "vnd.docker.reference.type"
	attestationManifestType  = "attestation-manifest"
)

// ImageInfo is the immutable metadata derived from one manifest digest.
type ImageInfo struct {
	Created   *time.Time
	Size      int64
	Platforms []string
}

// TagDetail reports metadata for one published tag.
type TagDetail struct {
	Tag       string   `json:"tag"`
	Digest    string   `json:"digest,omitempty"`
	Created   string   `json:"created,omitempty"`
	Size      int64    `json:"size,omitempty"`
	Platforms []string `json:"platforms,omitempty"`
	Error     string   `json:"error,omitempty"`
}

type registryDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations"`
	Platform    *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	} `json:"platform"`
}

type registryManifestDocument struct {
	MediaType string               `json:"mediaType"`
	Config    registryDescriptor   `json:"config"`
	Layers    []registryDescriptor `json:"layers"`
	Manifests []registryDescriptor `json:"manifests"`
}

var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// ValidDigest reports whether a string can be used as a sha256 registry digest.
func ValidDigest(value string) bool { return digestPattern.MatchString(value) }

// Read binds requests to the registry and credentials already approved by the caller.
type Read func(context.Context, string) (*http.Response, error)

var errRegistryImageUnavailable = errors.New("image metadata unavailable")

// ResolveImageInfo reads a manifest and verifies the config and any selected child digest.
func ResolveImageInfo(ctx context.Context, read Read, repository string, body []byte, mediaType string) (ImageInfo, error) {
	var document registryManifestDocument
	if err := json.Unmarshal(body, &document); err != nil {
		return ImageInfo{}, errRegistryImageUnavailable
	}
	if document.MediaType != "" {
		mediaType = document.MediaType
	}
	switch mediaType {
	case mediaTypeOCIIndex, mediaTypeDockerList:
		platforms, chosen := indexPlatforms(document.Manifests)
		if chosen == nil {
			return ImageInfo{}, errRegistryImageUnavailable
		}
		childBody, _, _, err := FetchManifest(ctx, read, repository, chosen.Digest, chosen.Digest)
		if err != nil {
			return ImageInfo{}, err
		}
		var child registryManifestDocument
		if err := json.Unmarshal(childBody, &child); err != nil || (child.MediaType != "" && child.MediaType != mediaTypeOCIManifest && child.MediaType != mediaTypeDockerManifest) {
			return ImageInfo{}, errRegistryImageUnavailable
		}
		info, err := imageInfoFromManifest(ctx, read, repository, child)
		if err != nil {
			return ImageInfo{}, err
		}
		info.Platforms = platforms
		return info, nil
	case mediaTypeOCIManifest, mediaTypeDockerManifest:
		return imageInfoFromManifest(ctx, read, repository, document)
	default:
		return ImageInfo{}, errRegistryImageUnavailable
	}
}

// indexPlatforms lists real platforms in an index, skipping attestation
// entries, and picks linux/amd64 or else the first real platform.
func indexPlatforms(manifests []registryDescriptor) ([]string, *registryDescriptor) {
	var platforms []string
	var chosen *registryDescriptor
	for i := range manifests {
		entry := &manifests[i]
		if entry.Platform == nil || entry.Platform.OS == "unknown" || entry.Annotations[attestationReferenceType] == attestationManifestType || !ValidDigest(entry.Digest) {
			continue
		}
		platforms = append(platforms, platformName(entry.Platform.OS, entry.Platform.Architecture, entry.Platform.Variant))
		if chosen == nil || (entry.Platform.OS == "linux" && entry.Platform.Architecture == "amd64" && (chosen.Platform.OS != "linux" || chosen.Platform.Architecture != "amd64")) {
			chosen = entry
		}
	}
	return platforms, chosen
}

func platformName(os, architecture, variant string) string {
	name := os + "/" + architecture
	if variant != "" {
		name += "/" + variant
	}
	return name
}

func imageInfoFromManifest(ctx context.Context, read Read, repository string, manifest registryManifestDocument) (ImageInfo, error) {
	if !ValidDigest(manifest.Config.Digest) || manifest.Config.Size < 0 || manifest.Config.Size > registryConfigLimit {
		return ImageInfo{}, errRegistryImageUnavailable
	}
	size := manifest.Config.Size
	for _, layer := range manifest.Layers {
		if layer.Size < 0 || layer.Size > math.MaxInt64-size {
			return ImageInfo{}, errRegistryImageUnavailable
		}
		size += layer.Size
	}
	response, err := read(ctx, "/v2/"+repository+"/blobs/"+manifest.Config.Digest)
	if err != nil {
		return ImageInfo{}, fmt.Errorf("fetch image config: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return ImageInfo{}, errRegistryImageUnavailable
	}
	body, err := readLimited(response.Body, registryConfigLimit)
	if err != nil || contentDigest(body) != manifest.Config.Digest {
		return ImageInfo{}, errRegistryImageUnavailable
	}
	var config struct {
		Created      *time.Time `json:"created"`
		OS           string     `json:"os"`
		Architecture string     `json:"architecture"`
		Variant      string     `json:"variant"`
	}
	if err := json.Unmarshal(body, &config); err != nil {
		return ImageInfo{}, errRegistryImageUnavailable
	}
	info := ImageInfo{Created: config.Created, Size: size}
	if config.OS != "" && config.Architecture != "" {
		info.Platforms = []string{platformName(config.OS, config.Architecture, config.Variant)}
	}
	return info, nil
}

// FetchManifest GETs a manifest by tag or digest and verifies that its
// content matches the digest reported by the registry (or the requested one).
func FetchManifest(ctx context.Context, read Read, repository, reference, expected string) ([]byte, string, string, error) {
	response, err := read(ctx, "/v2/"+repository+"/manifests/"+reference)
	if err != nil {
		return nil, "", "", fmt.Errorf("fetch manifest: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, "", "", errRegistryImageUnavailable
	}
	body, err := readLimited(response.Body, registryManifestLimit)
	if err != nil {
		return nil, "", "", err
	}
	digest := contentDigest(body)
	if reported := response.Header.Get("Docker-Content-Digest"); reported != "" && reported != digest {
		return nil, "", "", errRegistryImageUnavailable
	}
	if expected != "" && expected != digest {
		return nil, "", "", errRegistryImageUnavailable
	}
	mediaType, _, _ := strings.Cut(response.Header.Get("Content-Type"), ";")
	return body, strings.TrimSpace(mediaType), digest, nil
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read registry response: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, errRegistryImageUnavailable
	}
	return body, nil
}

func contentDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}
