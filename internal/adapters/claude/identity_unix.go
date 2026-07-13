//go:build darwin || linux

package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/tokemon/tokemon/internal/adapters"
)

func fileIdentity(adapterID, path string, info os.FileInfo) string {
	absolute, _ := filepath.Abs(path)
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return adapters.HashIdentity(fmt.Sprintf("%s:%s:%d:%d", adapterID, absolute, stat.Dev, stat.Ino))
	}
	return adapters.HashIdentity(adapterID + ":" + absolute)
}
