package art

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRepositoryManifest(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Join(filepath.Dir(filename), "..", "..", "web", "static", "tokemon")
	report, err := ValidateDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if report.Assets != 19 || report.MaxWidth != 640 || report.MaxHeight != 640 {
		t.Fatalf("unexpected art report: %+v", report)
	}
	files, err := StageFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != report.Assets || files[0] != "stage-00.png" || files[len(files)-1] != "stage-18.png" {
		t.Fatalf("stage files = %v", files)
	}
}

func TestValidateRejectsHashMutation(t *testing.T) {
	dir, data := writeFixture(t, 0, true)
	data[0] ^= 0xff
	if err := os.WriteFile(filepath.Join(dir, "stage-00.png"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ValidateDir(dir)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("validation error = %v, want hash mismatch", err)
	}
}

func TestValidateRejectsManifestMaxEdgeAboveDocumentedLimit(t *testing.T) {
	dir, _ := writeFixture(t, 0, true)
	manifestPath := filepath.Join(dir, manifestName)
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.MaxEdge = DefaultMaxEdge + 1
	manifestData, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, manifestData, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateDir(dir); err == nil || !strings.Contains(err.Error(), "exceeds documented maximum") {
		t.Fatalf("tampered max_edge error = %v", err)
	}
}

func TestValidateRejectsMissingAndOpaqueStages(t *testing.T) {
	dir, _ := writeFixture(t, 0, true)
	if err := os.Remove(filepath.Join(dir, "stage-00.png")); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateDir(dir); err == nil || !strings.Contains(err.Error(), "read stage-00.png") {
		t.Fatalf("missing asset error = %v", err)
	}

	dir, _ = writeFixture(t, 0, false)
	if _, err := ValidateDir(dir); err == nil || (!strings.Contains(err.Error(), "no transparent pixels") && !strings.Contains(err.Error(), "must be 8-bit RGBA")) {
		t.Fatalf("opaque asset error = %v", err)
	}
}

func TestValidateRejectsNonRGBAAndUnlistedStage(t *testing.T) {
	dir, data := writeFixture(t, 0, true)
	// A grayscale PNG is valid image data but violates the RGBA release contract.
	gray := image.NewGray(image.Rect(0, 0, 2, 2))
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, gray); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stage-00.png"), encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateDir(dir); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("non-RGBA asset error = %v", err)
	}

	dir, data = writeFixture(t, 0, true)
	if err := os.WriteFile(filepath.Join(dir, "stage-01.png"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateDir(dir); err == nil || !strings.Contains(err.Error(), "not present in manifest") {
		t.Fatalf("unlisted asset error = %v", err)
	}
}

func writeFixture(t *testing.T, stage int, transparent bool) (string, []byte) {
	t.Helper()
	dir := t.TempDir()
	imageData := image.NewRGBA(image.Rect(0, 0, 2, 2))
	fill := color.RGBA{R: 155, G: 187, B: 160, A: 255}
	if transparent {
		fill.A = 0
	}
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			imageData.SetRGBA(x, y, fill)
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, imageData); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	filename := fmt.Sprintf("stage-%02d.png", stage)
	if err := os.WriteFile(filepath.Join(dir, filename), data, 0o600); err != nil {
		t.Fatal(err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(data))
	manifest := Manifest{
		SchemaVersion: ManifestSchemaVersion,
		MaxEdge:       DefaultMaxEdge,
		Assets: map[string]Asset{
			fmt.Sprint(stage): {
				Form:     "fixture",
				Path:     assetURLPrefix + filename,
				SHA256:   hash,
				Width:    2,
				Height:   2,
				RGBA:     true,
				HasAlpha: true,
			},
		},
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifestName), manifestData, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, data
}
