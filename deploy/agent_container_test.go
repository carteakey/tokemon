package deploy

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAgentComposePreflightAndPermissionSmokeContracts(t *testing.T) {
	compose, err := os.ReadFile(filepath.Join("docker-compose.agent.example.yml"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(compose)
	for _, required := range []string{
		`command: ["agent", "--config", "/config/agent.env"]`,
		`user: "${TOKEMON_UID:-65532}:${TOKEMON_GID:-65532}"`,
		`${TOKEMON_AGENT_STATE_DIR:-./data/agent-state}:/state:rw`,
		`${TOKEMON_AGENT_CONFIG:?Set TOKEMON_AGENT_CONFIG to a mode-0600 agent.env}:/config/agent.env:ro`,
		`: > /state/state.db`,
		`touch "$$path/.tokemon-write-check"`,
	} {
		if !strings.Contains(content, required) {
			t.Errorf("agent Compose contract missing %q", required)
		}
	}
	if strings.Contains(content, "TOKEMON_INGEST_TOKEN:") || strings.Contains(content, "TOKEMON_DASHBOARD_TOKEN:") {
		t.Fatal("agent Compose exposes token fields as environment entries")
	}
	for _, provider := range []string{"TOKEMON_CLAUDE_DIR", "TOKEMON_CODEX_DIR", "TOKEMON_OPENCLAW_DIR", "TOKEMON_HERMES_DIR"} {
		if !strings.Contains(content, provider) || !strings.Contains(content, ":ro") {
			t.Fatalf("agent Compose provider mount contract missing for %s", provider)
		}
	}
}

func TestAgentContainerPreflightIsExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell preflight is not supported on Windows")
	}
	info, err := os.Stat(filepath.Join("agent-container-preflight.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("preflight is not executable: %v", info.Mode())
	}
}
