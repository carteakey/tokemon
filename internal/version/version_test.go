package version

import (
	"strings"
	"testing"
)

func TestInfoStringContainsDeploymentIdentity(t *testing.T) {
	info := Info{
		Version:   "0.3.0",
		Commit:    "abc123",
		BuildDate: "2026-07-22T12:00:00Z",
		GoVersion: "go1.26.0",
		OS:        "linux",
		Arch:      "arm64",
	}
	output := info.String()
	for _, want := range []string{
		"version: 0.3.0",
		"commit: abc123",
		"built: 2026-07-22T12:00:00Z",
		"platform: linux/arm64",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("version output does not contain %q: %s", want, output)
		}
	}
}
