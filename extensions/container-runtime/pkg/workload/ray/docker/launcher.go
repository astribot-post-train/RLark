package docker

import _ "embed"

// The launcher is executable material embedded into an immutable Docker
// manifest; no ConfigMap, host-wide ray stop, or daemon socket is needed.
//
//go:embed launcher.py
var launcher string

type launchConfig struct {
	Head           bool     `json:"head"`
	Start          []string `json:"start"`
	Address        string   `json:"address"`
	NodeAddress    string   `json:"nodeAddress"`
	Members        []string `json:"members"`
	Command        []string `json:"command,omitempty"`
	Prepare        string   `json:"prepare,omitempty"`
	Run            string   `json:"run,omitempty"`
	StartupSeconds int      `json:"startupSeconds"`
}
