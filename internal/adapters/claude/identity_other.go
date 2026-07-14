//go:build !darwin && !linux

package claude

import (
	"os"
	"path/filepath"

	"github.com/tokemon/tokemon/internal/adapters"
)

func fileIdentity(adapterID, path string, _ os.FileInfo) string {
	absolute, _ := filepath.Abs(path)
	return adapters.HashIdentity(adapterID + ":" + absolute)
}
