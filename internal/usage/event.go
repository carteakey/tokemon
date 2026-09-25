package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
)

const SchemaVersion = "1"

const (
	MaxEventIDLength      = 256
	MaxMachineIDLength    = 256
	MaxSessionIDLength    = 256
	MaxProjectLength      = 256
	MaxProviderLength     = 128
	MaxModelLength        = 256
	MaxCanonicalLength    = 256
	MaxToolLength         = 128
	MaxAdapterLength      = 128
	MaxIdentityLength     = 256
	MaxMetadataBytes      = 4 << 10
	MaxMetadataEntries    = 32
	MaxMetadataDepth      = 4
	MaxMetadataStringSize = 256
	MaxDurationMS         = int64(365 * 24 * time.Hour / time.Millisecond)
)

type Accuracy string

const (
	AccuracyReported  Accuracy = "reported"
	AccuracyDerived   Accuracy = "derived"
	AccuracyEstimated Accuracy = "estimated"
	AccuracyUnknown   Accuracy = "unknown"
)

func (a Accuracy) Valid() bool {
	switch a {
	case AccuracyReported, AccuracyDerived, AccuracyEstimated, AccuracyUnknown:
		return true
	default:
		return false
	}
}

type Source struct {
	Adapter        string `json:"adapter"`
	AdapterVersion string `json:"adapter_version"`
	Identity       string `json:"identity,omitempty"`
	Offset         int64  `json:"offset,omitempty"`
}

type Event struct {
	SchemaVersion    string         `json:"schema_version"`
	EventID          string         `json:"event_id"`
	Timestamp        time.Time      `json:"timestamp"`
	MachineID        string         `json:"machine_id"`
	SessionID        string         `json:"session_id,omitempty"`
	Project          string         `json:"project,omitempty"`
	Provider         string         `json:"provider"`
	Model            string         `json:"model"`
	CanonicalModel   string         `json:"canonical_model,omitempty"`
	Tool             string         `json:"tool"`
	InputTokens      *int64         `json:"input_tokens"`
	OutputTokens     *int64         `json:"output_tokens"`
	CacheReadTokens  *int64         `json:"cache_read_tokens"`
	CacheWriteTokens *int64         `json:"cache_write_tokens"`
	ReasoningTokens  *int64         `json:"reasoning_tokens"`
	TotalTokens      *int64         `json:"total_tokens"`
	DurationMS       *int64         `json:"duration_ms"`
	Cost             *float64       `json:"cost"`
	CostEstimated    bool           `json:"cost_estimated,omitempty"`
	Currency         string         `json:"currency"`
	TokenAccuracy    Accuracy       `json:"token_accuracy"`
	Source           Source         `json:"source"`
	Metadata         map[string]any `json:"metadata,omitempty"`
}

func (e Event) Validate() error {
	return e.validateAt(time.Now().UTC())
}

func (e Event) validateAt(now time.Time) error {
	if e.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version must be %q", SchemaVersion)
	}
	if err := validateIdentifier("event_id", e.EventID, MaxEventIDLength); err != nil {
		return err
	}
	if e.Timestamp.IsZero() {
		return errors.New("timestamp is required")
	}
	if e.Timestamp.Before(time.Unix(0, 0)) {
		return errors.New("timestamp cannot be before the Unix epoch")
	}
	if e.Timestamp.After(now.Add(24 * time.Hour)) {
		return errors.New("timestamp is too far in the future")
	}
	if err := validateText("machine_id", e.MachineID, MaxMachineIDLength, false); err != nil {
		return err
	}
	if err := validateText("session_id", e.SessionID, MaxSessionIDLength, true); err != nil {
		return err
	}
	if err := validateText("project", e.Project, MaxProjectLength, true); err != nil {
		return err
	}
	if strings.ContainsAny(e.Project, `/\\`) {
		return errors.New("project must be a label, not a filesystem path")
	}
	if err := validateText("provider", e.Provider, MaxProviderLength, true); err != nil {
		return err
	}
	if err := validateText("model", e.Model, MaxModelLength, true); err != nil {
		return err
	}
	if err := validateText("canonical_model", e.CanonicalModel, MaxCanonicalLength, true); err != nil {
		return err
	}
	if err := validateText("tool", e.Tool, MaxToolLength, true); err != nil {
		return err
	}
	if !e.TokenAccuracy.Valid() {
		return fmt.Errorf("invalid token_accuracy %q", e.TokenAccuracy)
	}
	for name, value := range map[string]*int64{
		"input_tokens": e.InputTokens, "output_tokens": e.OutputTokens,
		"cache_read_tokens": e.CacheReadTokens, "cache_write_tokens": e.CacheWriteTokens,
		"reasoning_tokens": e.ReasoningTokens, "total_tokens": e.TotalTokens,
		"duration_ms": e.DurationMS,
	} {
		if value != nil && *value < 0 {
			return fmt.Errorf("%s cannot be negative", name)
		}
	}
	if e.DurationMS != nil && *e.DurationMS > MaxDurationMS {
		return errors.New("duration_ms is too large")
	}
	if e.Cost != nil && (math.IsNaN(*e.Cost) || math.IsInf(*e.Cost, 0) || *e.Cost < 0) {
		return errors.New("cost must be finite and non-negative")
	}
	if e.Currency != "" && (len(e.Currency) != 3 || e.Currency != strings.ToUpper(e.Currency) || !isASCIIAlpha(e.Currency)) {
		return errors.New("currency must be a three-letter uppercase code")
	}
	if e.Source.Offset < 0 {
		return errors.New("source offset cannot be negative")
	}
	if err := validateText("source.adapter", e.Source.Adapter, MaxAdapterLength, false); err != nil {
		return err
	}
	if err := validateText("source.adapter_version", e.Source.AdapterVersion, MaxAdapterLength, true); err != nil {
		return err
	}
	if err := validateText("source.identity", e.Source.Identity, MaxIdentityLength, true); err != nil {
		return err
	}
	if e.TotalTokens != nil {
		var components int64
		for _, value := range []*int64{e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheWriteTokens} {
			if value == nil {
				continue
			}
			if components > math.MaxInt64-*value {
				return errors.New("token components overflow")
			}
			components += *value
		}
		if components > *e.TotalTokens {
			return errors.New("total_tokens is smaller than known token components")
		}
	}
	if e.Metadata != nil {
		payload, err := json.Marshal(e.Metadata)
		if err != nil || len(payload) > MaxMetadataBytes {
			return errors.New("metadata is too large or invalid")
		}
		if err := validateMetadata(e.Metadata, 0); err != nil {
			return err
		}
	}
	return nil
}

func validateText(name, value string, max int, allowWhitespace bool) error {
	if strings.TrimSpace(value) == "" {
		if name == "session_id" || name == "project" || name == "canonical_model" || name == "source.identity" {
			return nil
		}
		return fmt.Errorf("%s is required", name)
	}
	if len([]rune(value)) > max {
		return fmt.Errorf("%s is too long", name)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s cannot have leading or trailing whitespace", name)
	}
	for _, r := range value {
		if unicode.IsControl(r) || (!allowWhitespace && unicode.IsSpace(r)) {
			return fmt.Errorf("%s contains invalid characters", name)
		}
	}
	return nil
}

func validateIdentifier(name, value string, max int) error {
	if err := validateText(name, value, max, false); err != nil {
		return err
	}
	return nil
}

func isASCIIAlpha(value string) bool {
	for _, r := range value {
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') {
			return false
		}
	}
	return true
}

func validateMetadata(value any, depth int) error {
	if depth > MaxMetadataDepth {
		return errors.New("metadata nesting is too deep")
	}
	switch typed := value.(type) {
	case map[string]any:
		if len(typed) > MaxMetadataEntries {
			return errors.New("metadata has too many entries")
		}
		for key, child := range typed {
			if err := validateMetadataKey(key); err != nil {
				return err
			}
			if err := validateMetadata(child, depth+1); err != nil {
				return err
			}
		}
	case []any:
		if len(typed) > MaxMetadataEntries {
			return errors.New("metadata array has too many entries")
		}
		for _, child := range typed {
			if err := validateMetadata(child, depth+1); err != nil {
				return err
			}
		}
	case string:
		if len([]rune(typed)) > MaxMetadataStringSize {
			return errors.New("metadata string is too long")
		}
		for _, r := range typed {
			if unicode.IsControl(r) {
				return errors.New("metadata contains control characters")
			}
		}
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return errors.New("metadata number must be finite")
		}
	case nil, bool, int, int64, uint64:
		// JSON decoding normally yields only nil, bool, float64, string, maps,
		// and arrays. The integer cases keep direct callers safe as well.
	default:
		return errors.New("metadata contains an unsupported value")
	}
	return nil
}

func validateMetadataKey(key string) error {
	if strings.TrimSpace(key) == "" || len([]rune(key)) > MaxMetadataStringSize {
		return errors.New("metadata key is empty or too long")
	}
	for _, r := range key {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return errors.New("metadata key contains invalid characters")
		}
	}
	lower := strings.ToLower(key)
	for _, sensitive := range []string{"prompt", "response", "content", "source", "path", "title", "secret", "credential", "token", "repository", "repo", "code", "message", "conversation", "command", "cwd"} {
		if strings.Contains(lower, sensitive) {
			return errors.New("metadata key is not permitted")
		}
	}
	return nil
}

func DeterministicID(machineID, adapter, sourceIdentity string, offset int64, timestamp, sessionID string) string {
	input := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s\x00%s", machineID, adapter, sourceIdentity, offset, timestamp, sessionID)
	hash := sha256.Sum256([]byte(input))
	return "sha256:" + hex.EncodeToString(hash[:])
}

// NormalizeSessionID preserves opaque provider IDs while replacing path-,
// title-, and credential-like values with a deterministic non-sensitive ID.
// Adapters use HashSessionID for filename fallbacks so a raw basename never
// becomes an outbound session identifier.
func NormalizeSessionID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if isSafeSessionID(value) {
		return value
	}
	return HashSessionID(value)
}

// HashSessionID returns a stable opaque ID for a provider value that cannot be
// safely allowlisted. The input is never included in the returned value.
func HashSessionID(value string) string {
	hash := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(hash[:])
}

func isSafeSessionID(value string) bool {
	if len([]rune(value)) > MaxSessionIDLength || strings.ContainsAny(value, "/\\\r\n\t ") {
		return false
	}
	if strings.HasPrefix(value, "sha256:") {
		if len(value) != len("sha256:")+sha256.Size*2 {
			return false
		}
		_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
		return err == nil
	}
	lower := strings.ToLower(value)
	for _, prefix := range []string{"bearer ", "sk-", "ghp_", "github_pat_"} {
		if strings.HasPrefix(lower, prefix) {
			return false
		}
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func Int64(value int64) *int64 { return &value }

func Float64(value float64) *float64 { return &value }

// NormalizeProject reduces a working directory or explicit project label to a
// case-insensitive basename. Full filesystem paths must never leave an agent.
func NormalizeProject(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	value = strings.TrimRight(value, "/")
	if value == "" {
		return ""
	}
	parts := strings.Split(value, "/")
	name := strings.TrimSpace(parts[len(parts)-1])
	if name == "" || name == "." || name == ".." {
		return ""
	}
	return strings.ToLower(name)
}
