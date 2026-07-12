package main

import (
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
