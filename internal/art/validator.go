// Package art validates the checked-in evolution art contract.
//
// The validator deliberately uses only the standard library so it can run in
// a release checkout before the dashboard is built or deployed.
package art

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	// ManifestSchemaVersion is the version of the checked-in art manifest.
	ManifestSchemaVersion = 1
	// DefaultMaxEdge is the largest encoded edge accepted by the art pass.
	DefaultMaxEdge = 640
	manifestName   = "manifest.json"
	assetURLPrefix = "/static/tokemon/"
)

// Manifest is the stable, checked-in lookup contract for evolution art.
type Manifest struct {
	SchemaVersion int              `json:"schema_version"`
	MaxEdge       int              `json:"max_edge"`
	Assets        map[string]Asset `json:"assets"`
}

// Asset records both the stable URL and the release-time bytes expected at
// that URL. Dimensions and alpha requirements make accidental opaque or
// oversized replacements fail before they reach a release.
type Asset struct {
	Form     string `json:"form"`
	Path     string `json:"path"`
	SHA256   string `json:"sha256"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	RGBA     bool   `json:"rgba"`
	HasAlpha bool   `json:"has_alpha"`
}

// Report contains the useful summary from a successful validation.
type Report struct {
	Assets    int
	MaxWidth  int
	MaxHeight int
}

// ValidateDir validates manifest.json and every stage-*.png in dir.
func ValidateDir(dir string) (Report, error) {
	manifest, err := loadManifest(filepath.Join(dir, manifestName))
	if err != nil {
		return Report{}, err
	}
	if manifest.SchemaVersion != ManifestSchemaVersion {
		return Report{}, fmt.Errorf("art manifest schema_version must be %d", ManifestSchemaVersion)
	}
	if manifest.MaxEdge <= 0 {
		return Report{}, fmt.Errorf("art manifest max_edge must be positive")
	}
	if manifest.MaxEdge > DefaultMaxEdge {
		return Report{}, fmt.Errorf("art manifest max_edge %d exceeds documented maximum %d", manifest.MaxEdge, DefaultMaxEdge)
	}
	if len(manifest.Assets) == 0 {
		return Report{}, fmt.Errorf("art manifest assets must not be empty")
	}

	stages := make(map[int]struct{}, len(manifest.Assets))
	paths := make(map[string]string, len(manifest.Assets))
	report := Report{Assets: len(manifest.Assets)}
	for key, asset := range manifest.Assets {
		stage, err := strconv.Atoi(key)
		if err != nil || stage < 0 {
			return Report{}, fmt.Errorf("art manifest stage key %q must be a non-negative integer", key)
		}
		if key != strconv.Itoa(stage) {
			return Report{}, fmt.Errorf("art manifest stage key %q is not canonical", key)
		}
		if _, exists := stages[stage]; exists {
			return Report{}, fmt.Errorf("art manifest repeats stage %d", stage)
		}
		stages[stage] = struct{}{}
		expectedURL := fmt.Sprintf("%sstage-%02d.png", assetURLPrefix, stage)
		if asset.Path != expectedURL {
			return Report{}, fmt.Errorf("stage %d path %q does not match %q", stage, asset.Path, expectedURL)
		}
		if previous, exists := paths[asset.Path]; exists {
			return Report{}, fmt.Errorf("stage %s reuses asset path already used by stage %s", key, previous)
		}
		paths[asset.Path] = key
		if strings.TrimSpace(asset.Form) == "" {
			return Report{}, fmt.Errorf("stage %d form must not be empty", stage)
		}
		if len(asset.SHA256) != sha256.Size*2 {
			return Report{}, fmt.Errorf("stage %d sha256 must be a %d-character hex digest", stage, sha256.Size*2)
		}
		if _, err := hex.DecodeString(asset.SHA256); err != nil {
			return Report{}, fmt.Errorf("stage %d sha256 is not valid hex: %w", stage, err)
		}
		if asset.Width <= 0 || asset.Height <= 0 {
			return Report{}, fmt.Errorf("stage %d dimensions must be positive", stage)
		}
		if asset.Width > manifest.MaxEdge || asset.Height > manifest.MaxEdge {
			return Report{}, fmt.Errorf("stage %d dimensions %dx%d exceed max edge %d", stage, asset.Width, asset.Height, manifest.MaxEdge)
		}
		if !asset.RGBA || !asset.HasAlpha {
			return Report{}, fmt.Errorf("stage %d must declare rgba and has_alpha", stage)
		}

		filename := filepath.Join(dir, filepath.Base(asset.Path))
		data, err := os.ReadFile(filename)
		if err != nil {
			return Report{}, fmt.Errorf("stage %d read %s: %w", stage, filepath.Base(asset.Path), err)
		}
		actualHash := fmt.Sprintf("%x", sha256.Sum256(data))
		if actualHash != strings.ToLower(asset.SHA256) {
			return Report{}, fmt.Errorf("stage %d sha256 mismatch: manifest %s, actual %s", stage, asset.SHA256, actualHash)
		}
		width, height, hasAlpha, err := inspectPNG(data)
		if err != nil {
			return Report{}, fmt.Errorf("stage %d %s: %w", stage, filepath.Base(asset.Path), err)
		}
		if width != asset.Width || height != asset.Height {
			return Report{}, fmt.Errorf("stage %d dimensions mismatch: manifest %dx%d, actual %dx%d", stage, asset.Width, asset.Height, width, height)
		}
		if !hasAlpha {
			return Report{}, fmt.Errorf("stage %d %s has no transparent pixels", stage, filepath.Base(asset.Path))
		}
		if width > report.MaxWidth {
			report.MaxWidth = width
		}
		if height > report.MaxHeight {
			report.MaxHeight = height
		}
	}

	for stage := 0; stage < len(stages); stage++ {
		if _, exists := stages[stage]; !exists {
			return Report{}, fmt.Errorf("art manifest is missing stage %d", stage)
		}
	}
	if err := validateStageCoverage(dir, stages); err != nil {
		return Report{}, err
	}
	return report, nil
}

func loadManifest(filename string) (Manifest, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return Manifest{}, fmt.Errorf("read %s: %w", filename, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode %s: %w", filename, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Manifest{}, fmt.Errorf("decode %s: trailing JSON", filename)
		}
		return Manifest{}, fmt.Errorf("decode %s: trailing JSON: %w", filename, err)
	}
	return manifest, nil
}

func validateStageCoverage(dir string, stages map[int]struct{}) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read art directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "stage-") || !strings.HasSuffix(entry.Name(), ".png") {
			continue
		}
		stageText := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "stage-"), ".png")
		stage, err := strconv.Atoi(stageText)
		if err != nil {
			return fmt.Errorf("stage asset %q is not numbered", entry.Name())
		}
		expectedName := fmt.Sprintf("stage-%02d.png", stage)
		if entry.Name() != expectedName {
			return fmt.Errorf("stage asset %q does not use stable filename %q", entry.Name(), expectedName)
		}
		if _, exists := stages[stage]; !exists {
			return fmt.Errorf("stage asset %q is not present in manifest", entry.Name())
		}
	}
	return nil
}

func inspectPNG(data []byte) (width, height int, hasAlpha bool, err error) {
	const (
		pngSignatureLength = 8
		ihdrLength         = 13
		ihdrChunkLength    = 4
		ihdrChunkType      = 4
		ihdrDataOffset     = pngSignatureLength + ihdrChunkLength + ihdrChunkType
	)
	if len(data) < ihdrDataOffset+ihdrLength || !bytes.Equal(data[:pngSignatureLength], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
		return 0, 0, false, fmt.Errorf("not a PNG")
	}
	if binary.BigEndian.Uint32(data[8:12]) != ihdrLength || string(data[12:16]) != "IHDR" {
		return 0, 0, false, fmt.Errorf("missing PNG IHDR")
	}
	if data[24] != 8 || data[25] != 6 {
		return 0, 0, false, fmt.Errorf("must be 8-bit RGBA PNG (bit depth %d, color type %d)", data[24], data[25])
	}
	decoded, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return 0, 0, false, err
	}
	width, height = decoded.Bounds().Dx(), decoded.Bounds().Dy()
	for y := decoded.Bounds().Min.Y; y < decoded.Bounds().Max.Y && !hasAlpha; y++ {
		for x := decoded.Bounds().Min.X; x < decoded.Bounds().Max.X; x++ {
			_, _, _, alpha := decoded.At(x, y).RGBA()
			if alpha < 0xffff {
				hasAlpha = true
				break
			}
		}
	}
	return width, height, hasAlpha, nil
}

// StageFiles returns the numbered stage filenames in deterministic order.
// It is useful to keep release logs and visual-QA output stable.
func StageFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "stage-") && strings.HasSuffix(entry.Name(), ".png") {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(files)
	return files, nil
}
