package clusterupgrade

import (
	"errors"
	"strings"
	"testing"
)

const digest = "sha256:1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"

const imagesConfig = `apiVersion: rlark.io/v1alpha1
kind: DeployConfig
plane: data
kubernetes:
  agent-image: registry.example/agent@` + digest + `
  image: registry.example/tools@` + digest + "\n"

func TestParseImages(t *testing.T) {
	want := Images{"registry.example/agent@" + digest, "registry.example/tools@" + digest}
	if got, err := ParseImages(imagesConfig); err != nil || got != want {
		t.Fatalf("parse = %+v, %v", got, err)
	}
	for _, input := range []string{
		imagesConfig + "  replicas: 3\n",
		imagesConfig + "  agent-image: registry.example/other@" + digest + "\n",
		imagesConfig + "---\nplane: data\n",
		strings.Replace(imagesConfig, "@"+digest, ":latest", 1),
	} {
		if _, err := ParseImages(input); err == nil {
			t.Fatalf("accepted invalid configuration: %s", input)
		}
	}
	_, err := ParseImages(imagesConfig + "  image-pull-secrets: [secret]\n")
	var field *FieldError
	if !errors.As(err, &field) || field.Field != "kubernetes.image-pull-secrets" {
		t.Fatalf("unexpected rejected field: %v", err)
	}
}

func TestPublicMessageFiltersUnstructuredTerminationOutput(t *testing.T) {
	if got := PublicMessage(`{"kind":"unsupported-field", "field":"kubernetes.image-pull-secrets"}`); got != "unsupported DeployConfig field kubernetes.image-pull-secrets" {
		t.Fatalf("message = %q", got)
	}
	for _, input := range []string{"Bearer credential", `{"kind":"unknown","field":"secret"}`, `{"kind":"unsupported-field","field":"token with whitespace"}`} {
		if got := PublicMessage(input); got != "" {
			t.Fatalf("exposed arbitrary termination output %q", got)
		}
	}
}
