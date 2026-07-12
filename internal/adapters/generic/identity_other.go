//go:build !darwin && !linux

package generic

import (
	"os"
	"path/filepath"

	"github.com/tokemon/tokemon/internal/adapters"
)

func sourceIdentity(path string, _ os.FileInfo) string {
	absolute, _ := filepath.Abs(path)
	return adapters.HashIdentity(absolute)
}
