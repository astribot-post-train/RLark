// Package addoncatalog embeds the Addon definitions shipped by Astribot.
package addoncatalog

import (
	"embed"
	"io/fs"
)

//go:embed catalog/*/addon.yaml catalog/*/manifests catalog/*/bootstrap
var files embed.FS

// Files returns the read-only Astribot catalog.
func Files() fs.FS {
	return files
}
