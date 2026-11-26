package clusterupgrade

import (
	_ "crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/distribution/reference"
	yaml "go.yaml.in/yaml/v3"
)

// DriverAnnotation is set in the management Cluster's deployment declaration.
// An entry selects the image-only Astribot protocol instead of the stock install.
const DriverAnnotation = "rlark.io/upgrade-driver"
const DriverImageOnly = "astribot-image-only"

type Images struct {
	Agent string
	Tools string
}

type FieldError struct{ Field string }

func (e *FieldError) Error() string { return "unsupported DeployConfig field " + e.Field }

// ParseImages accepts only image changes; the executor reads every other field
// from the live Kubernetes objects. Require immutable image references to make
// readiness checks and rollback deterministic.
func ParseImages(input string) (Images, error) {
	var document yaml.Node
	decoder := yaml.NewDecoder(strings.NewReader(input))
	if err := decoder.Decode(&document); err != nil {
		return Images{}, errors.New("invalid upgrade DeployConfig YAML")
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Images{}, errors.New("upgrade DeployConfig must contain one YAML document")
	}
	if len(document.Content) != 1 {
		return Images{}, errors.New("upgrade DeployConfig is required")
	}
	root, err := fields(document.Content[0], "", "apiVersion", "kind", "plane", "kubernetes")
	if err != nil {
		return Images{}, err
	}
	if !isString(root["apiVersion"], "rlark.io/v1alpha1") || !isString(root["kind"], "DeployConfig") || !isString(root["plane"], "data") {
		return Images{}, errors.New("expected a data-plane rlark.io/v1alpha1 DeployConfig")
	}
	images, err := fields(root["kubernetes"], "kubernetes.", "agent-image", "image")
	if err != nil {
		return Images{}, err
	}
	agent, err := imageReference(images["agent-image"], "agent-image")
	if err != nil {
		return Images{}, err
	}
	tools, err := imageReference(images["image"], "image")
	if err != nil {
		return Images{}, err
	}
	return Images{Agent: agent, Tools: tools}, nil
}

func fields(n *yaml.Node, prefix string, allowed ...string) (map[string]*yaml.Node, error) {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, errors.New("invalid upgrade DeployConfig mapping")
	}
	out := make(map[string]*yaml.Node, len(n.Content)/2)
	for i := 0; i < len(n.Content); i += 2 {
		key := n.Content[i]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || !fieldName.MatchString(strings.ToLower(key.Value)) {
			return nil, errors.New("invalid upgrade DeployConfig field")
		}
		if _, duplicate := out[key.Value]; duplicate {
			return nil, errors.New("duplicate upgrade DeployConfig field")
		}
		known := false
		for _, name := range allowed {
			if name == key.Value {
				known = true
				break
			}
		}
		if !known {
			return nil, &FieldError{Field: prefix + key.Value}
		}
		out[key.Value] = n.Content[i+1]
	}
	return out, nil
}

func isString(n *yaml.Node, want string) bool {
	return n != nil && n.Kind == yaml.ScalarNode && n.Tag == "!!str" && n.Value == want
}

func imageReference(n *yaml.Node, field string) (string, error) {
	if n != nil && n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
		if image, err := pinnedImage(n.Value); err == nil {
			return image, nil
		}
	}
	return "", fmt.Errorf("DeployConfig kubernetes.%s must be an image pinned by sha256 digest", field)
}

func pinnedImage(value string) (string, error) {
	if named, err := reference.ParseNormalizedNamed(value); err == nil {
		if digested, ok := named.(reference.Digested); ok && digested.Digest().Algorithm().String() == "sha256" {
			return value, nil
		}
	}
	return "", errors.New("expected an image pinned by sha256 digest")
}
