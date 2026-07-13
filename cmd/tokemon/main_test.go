package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCatalogValidate(t *testing.T) {
	path := filepath.Join("..", "..", "catalog", "models.yaml")
	if err := run([]string{"catalog", "validate", "--catalog", path}); err != nil {
		t.Fatal(err)
	}
}

func TestRunCatalogValidateRejectsInvalidCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(path, []byte("schema_version: 1\nmodels: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"catalog", "validate", "--catalog", path})
	if err == nil || !strings.Contains(err.Error(), `schema_version must be "2"`) {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestApplyServerConfigPrecedence(t *testing.T) {
	t.Setenv("TOKEMON_SERVER_ADDR", "")
	t.Setenv("TOKEMON_DATABASE", "")
	t.Setenv("TOKEMON_INGEST_TOKEN", "")
	t.Setenv("TOKEMON_MODEL_CATALOG", "")

	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := flags.String("addr", ":8080", "")
	databasePath := flags.String("database", "tokemon.db", "")
	ingestToken := flags.String("ingest-token", "", "")
	catalogPath := flags.String("catalog", "catalog/models.yaml", "")
	if err := flags.Parse([]string{}); err != nil {
		t.Fatal(err)
	}
	applyServerConfig(flags, map[string]string{
		"TOKEMON_SERVER_ADDR":   "0.0.0.0:18080",
		"TOKEMON_DATABASE":      "/tmp/tokemon.db",
		"TOKEMON_INGEST_TOKEN":  "file-token",
		"TOKEMON_MODEL_CATALOG": "/tmp/models.yaml",
	}, addr, databasePath, ingestToken, catalogPath)
	if *addr != "0.0.0.0:18080" || *databasePath != "/tmp/tokemon.db" || *ingestToken != "file-token" || *catalogPath != "/tmp/models.yaml" {
		t.Fatalf("config values not applied: addr=%q database=%q token=%q catalog=%q", *addr, *databasePath, *ingestToken, *catalogPath)
	}

	if err := flags.Set("addr", "127.0.0.1:9999"); err != nil {
		t.Fatal(err)
	}
	applyServerConfig(flags, map[string]string{"TOKEMON_SERVER_ADDR": "0.0.0.0:18080"}, addr, databasePath, ingestToken, catalogPath)
	if *addr != "127.0.0.1:9999" {
		t.Fatalf("explicit flag was overwritten: %q", *addr)
	}
}
