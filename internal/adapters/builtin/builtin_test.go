package builtin

import (
	"strings"
	"testing"
)

func TestDefaultSelectionKeepsNativeAdaptersAndAddsConfiguredGenericJSONL(t *testing.T) {
	selected, err := Select(Config{Home: t.TempDir(), JSONLPaths: []string{"usage.jsonl"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 8 {
		t.Fatalf("selected %d adapters, want 8: %+v", len(selected), selected)
	}
	if selected[len(selected)-1].ID != "generic-jsonl" {
		t.Fatalf("generic adapter is not last in stable registry order: %+v", selected)
	}
}

func TestExplicitSelectionRejectsUnknownAndPreservesRegistryOrder(t *testing.T) {
	selected, err := Select(Config{Home: t.TempDir()}, []string{"openclaw", "claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{selected[0].ID, selected[1].ID}; got[0] != "claude-code" || got[1] != "openclaw" {
		t.Fatalf("selection order = %v, want registry order", got)
	}
	if _, err := Select(Config{}, []string{"missing"}); err == nil || !strings.Contains(err.Error(), "unknown adapter") {
		t.Fatalf("unknown adapter error = %v", err)
	}
}
