//go:build darwin || linux

package generic

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/tokemon/tokemon/internal/adapters"
)

func sourceIdentity(path string, info os.FileInfo) string {
	absolute, _ := filepath.Abs(path)
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return adapters.HashIdentity(fmt.Sprintf("%s:%d:%d", absolute, stat.Dev, stat.Ino))
	}
	return adapters.HashIdentity(absolute)
}
