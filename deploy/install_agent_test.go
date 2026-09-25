package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallAgentCleanInstallAndUpgradeProtectSecretsAndState(t *testing.T) {
	home := t.TempDir()
	installDir := filepath.Join(home, "bin")
	statePath := filepath.Join(home, "state", "state.db")
	fakeBinary := filepath.Join(t.TempDir(), "tokemon")
	if err := os.WriteFile(fakeBinary, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	runInstaller := func(token string) string {
		t.Helper()
		cmd := exec.Command("bash", "install-agent.sh",
			"--binary", fakeBinary,
			"--server", "https://hub.example.test",
			"--machine-id", "clean-machine",
			"--home", home,
			"--state", statePath,
			"--install-dir", installDir,
			"--no-supervisor",
		)
		cmd.Env = append(os.Environ(), "HOME="+home, "TOKEMON_HOME=", "TOKEMON_INGEST_TOKEN="+token, "TOKEMON_SERVER_URL=")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("installer failed: %v\n%s", err, output)
		}
		if strings.Contains(string(output), token) {
			t.Fatalf("installer output exposed token: %q", output)
		}
		return string(output)
	}

	firstToken := "first-secret-value"
	runInstaller(firstToken)
	configPath := filepath.Join(home, ".config", "tokemon", "agent.env")
	assertMode0600(t, configPath)
	assertMode0600(t, statePath)
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "TOKEMON_INGEST_TOKEN="+firstToken) {
		t.Fatalf("config did not contain configured token: %q", config)
	}
	stateBefore, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	secondToken := "second-secret-value"
	runInstaller(secondToken)
	assertMode0600(t, configPath)
	assertMode0600(t, statePath)
	stateAfter, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(stateAfter) != string(stateBefore) {
		t.Fatalf("upgrade changed existing state database")
	}
	config, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "TOKEMON_INGEST_TOKEN="+secondToken) || strings.Contains(string(config), firstToken) {
		t.Fatalf("upgrade did not replace token safely: %q", config)
	}

}

func TestInstallAgentRejectsTokenNewlinesBeforeWritingConfig(t *testing.T) {
	home := t.TempDir()
	fakeBinary := filepath.Join(t.TempDir(), "tokemon")
	if err := os.WriteFile(fakeBinary, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "install-agent.sh",
		"--binary", fakeBinary,
		"--server", "https://hub.example.test",
		"--home", home,
		"--no-supervisor",
	)
	cmd.Env = append(os.Environ(), "HOME="+home, "TOKEMON_HOME=", "TOKEMON_INGEST_TOKEN=bad\nsecret")
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "token contains a newline") {
		t.Fatalf("newline token result = %v, output = %q", err, output)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "tokemon", "agent.env")); !os.IsNotExist(err) {
		t.Fatalf("installer wrote config after rejecting token: %v", err)
	}
}

func assertMode0600(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("%s mode = %o, want 600", path, got)
	}
}
