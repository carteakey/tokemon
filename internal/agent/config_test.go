package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadEnvFileIsDataOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.env")
	if err := os.WriteFile(path, []byte("# comment\nexport TOKEMON_SERVER_URL=\"https://example.test\"\nTOKEMON_INGEST_TOKEN='secret'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := ReadEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if values["TOKEMON_SERVER_URL"] != "https://example.test" || values["TOKEMON_INGEST_TOKEN"] != "secret" {
		t.Fatalf("unexpected values: %#v", values)
	}
}

func TestReadEnvFileRejectsMalformedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.env")
	if err := os.WriteFile(path, []byte("not an assignment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadEnvFile(path); err == nil {
		t.Fatal("expected malformed config error")
	}
}

func TestDefaultServerConfigPath(t *testing.T) {
	want := filepath.Join("/tmp", ".config", "tokemon", "server.env")
	if got := DefaultServerConfigPath("/tmp"); got != want {
		t.Fatalf("DefaultServerConfigPath() = %q, want %q", got, want)
	}
}

func TestDefaultStatePath(t *testing.T) {
	want := filepath.Join("/tmp", ".local", "share", "tokemon", "state.db")
	if got := DefaultStatePath("/tmp"); got != want {
		t.Fatalf("DefaultStatePath() = %q, want %q", got, want)
	}
}
