package registry

// ImagePullSecret is a registry credential in the format used by the member installer.
type ImagePullSecret struct {
	Name             string `json:"name"`
	DockerConfigJSON string `json:"docker_config_json"`
}
