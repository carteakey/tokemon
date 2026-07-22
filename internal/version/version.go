// Package version contains build identity shared by the CLI and agent.
package version

import (
	"fmt"
	"runtime"
	"strings"
)

// These values are replaced by release builds with -ldflags. Keeping useful
// defaults makes local development and source builds self-describing too.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// Info is the non-secret runtime identity reported by the CLI and agent.
type Info struct {
	Version   string
	Commit    string
	BuildDate string
	GoVersion string
	OS        string
	Arch      string
}

func Current() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		BuildDate: BuildDate,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
}

func (i Info) String() string {
	return strings.Join([]string{
		fmt.Sprintf("version: %s", valueOrUnknown(i.Version)),
		fmt.Sprintf("commit: %s", valueOrUnknown(i.Commit)),
		fmt.Sprintf("built: %s", valueOrUnknown(i.BuildDate)),
		fmt.Sprintf("go: %s", valueOrUnknown(i.GoVersion)),
		fmt.Sprintf("platform: %s/%s", valueOrUnknown(i.OS), valueOrUnknown(i.Arch)),
	}, "\n")
}

func valueOrUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	return value
}
