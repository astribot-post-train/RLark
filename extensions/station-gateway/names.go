package main

import (
	"fmt"
	"os"
	"strings"
)

// namesMarker ends every line of the node's hosts file that this program
// owns; the installer writes the first ones in the same form (lib.sh).
const namesMarker = " # rlark names"

// writeNames makes the marked lines of the hosts file the given names and
// leaves every other line alone. The file is rewritten in place: host-network
// pods have this very file mounted, and a new file under its name would leave
// them with the old one.
func writeNames(path string, names []hostName) error {
	current, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(string(current), "\n"), "\n") {
		if !strings.HasSuffix(line, namesMarker) {
			lines = append(lines, line)
		}
	}
	for _, name := range names {
		lines = append(lines, fmt.Sprintf("%s %s%s", name.Address, name.Name, namesMarker))
	}
	wanted := strings.Join(lines, "\n") + "\n"
	if wanted == string(current) {
		return nil
	}
	return os.WriteFile(path, []byte(wanted), 0644)
}
