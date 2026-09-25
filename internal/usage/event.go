package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const SchemaVersion = "1"

const (
	maxEventIDLength   = 200
	maxMachineIDLength = 128
	maxSessionIDLength = 256
	maxProjectLength   = 512
	maxProviderLength  = 128
	maxModelLength     = 256
	maxToolLength      = 128
	maxAdapterLength   = 128
	maxAdapterVersion  = 64
	maxSourceIdentity  = 1024
	currencyCodeLength = 3
	timestampGrace     = 1 * time.Minute
	oldestEventYear    = 2000
)

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// eventIDPrefix identifies deterministic IDs produced by DeterministicID.
const eventIDPrefix = "sha256:"

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
	if e.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version must be %q", SchemaVersion)
	}
	if !validEventID(e.EventID) {
		return fmt.Errorf("event_id must be a non-empty string of at most %d US-ASCII characters", maxEventIDLength)
	}
	if e.Timestamp.IsZero() {
		return errors.New("timestamp is required")
	}
	if e.Timestamp.Before(time.Date(oldestEventYear, 1, 1, 0, 0, 0, 0, time.UTC)) {
		return fmt.Errorf("timestamp %s predates the supported range", e.Timestamp.UTC().Format(time.RFC3339))
	}
	if e.Timestamp.After(time.Now().Add(timestampGrace)) {
		return fmt.Errorf("timestamp %s is in the future", e.Timestamp.UTC().Format(time.RFC3339))
	}
	if strings.TrimSpace(e.MachineID) == "" || len(e.MachineID) > maxMachineIDLength {
		return fmt.Errorf("machine_id must be a non-empty string of at most %d characters", maxMachineIDLength)
	}
	if len(e.SessionID) > maxSessionIDLength {
		return fmt.Errorf("session_id must be at most %d characters", maxSessionIDLength)
	}
	if strings.TrimSpace(e.Provider) == "" || len(e.Provider) > maxProviderLength {
		return fmt.Errorf("provider must be a non-empty string of at most %d characters", maxProviderLength)
	}
	if strings.TrimSpace(e.Model) == "" || len(e.Model) > maxModelLength {
		return fmt.Errorf("model must be a non-empty string of at most %d characters", maxModelLength)
	}
	if len(e.CanonicalModel) > maxModelLength {
		return fmt.Errorf("canonical_model must be at most %d characters", maxModelLength)
	}
	if strings.TrimSpace(e.Tool) == "" || len(e.Tool) > maxToolLength {
		return fmt.Errorf("tool must be a non-empty string of at most %d characters", maxToolLength)
	}
	if len(e.Project) > maxProjectLength {
		return fmt.Errorf("project must be at most %d characters", maxProjectLength)
	}
	if !e.TokenAccuracy.Valid() {
		return fmt.Errorf("invalid token_accuracy %q", e.TokenAccuracy)
	}
	if len(e.Metadata) > 0 {
		return errors.New("metadata is not allowed in outbound usage events")
	}
	if _, err := validateCurrency(e.Currency); err != nil {
		return err
	}
	if len(e.Source.Adapter) > maxAdapterLength || len(e.Source.AdapterVersion) > maxAdapterVersion || len(e.Source.Identity) > maxSourceIdentity {
		return errors.New("source adapter, adapter version, or identity exceeds the length limit")
	}
	if !validTokenTotals(e) {
		return errors.New("total_tokens must be at least as large as every reported token component")
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
	if e.Cost != nil && *e.Cost < 0 {
		return errors.New("cost cannot be negative")
	}
	return nil
}

// SanitizeOutbound removes the extensible metadata bag before an event is
// serialized for a hub. The normalized schema is intentionally allowlisted so
// a generic source cannot accidentally upload prompts, responses, paths, or
// other provider payloads. Provider totals that contradict a reported token
// component become unknown rather than blocking every otherwise-valid event in
// the same collection pass.
func SanitizeOutbound(event Event) Event {
	event.Metadata = nil
	if event.TotalTokens != nil && !validTokenTotals(event) {
		event.TotalTokens = nil
		event.TokenAccuracy = AccuracyUnknown
	}
	return event
}

// DeterministicID produces a stable, content-addressed event identifier. Do
// not change its input construction: stored events depend on it for idempotent
// re-ingestion.
func DeterministicID(machineID, adapter, sourceIdentity string, offset int64, timestamp, sessionID string) string {
	input := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s\x00%s", machineID, adapter, sourceIdentity, offset, timestamp, sessionID)
	hash := sha256.Sum256([]byte(input))
	return "sha256:" + hex.EncodeToString(hash[:])
}

func validEventID(value string) bool {
	if value == "" || len(value) > maxEventIDLength {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character <= ' ' || character > '~' {
			return false
		}
	}
	if strings.HasPrefix(value, eventIDPrefix) {
		digest := strings.TrimPrefix(value, eventIDPrefix)
		return len(digest) == sha256.Size*2 && isHex(digest)
	}
	return true
}

func isHex(value string) bool {
	for index := 0; index < len(value); index++ {
		character := value[index]
		if !(character >= '0' && character <= '9') && !(character >= 'a' && character <= 'f') && !(character >= 'A' && character <= 'F') {
			return false
		}
	}
	return true
}

func validateCurrency(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !currencyPattern.MatchString(value) {
		return "", fmt.Errorf("currency must be a 3-letter ISO code in uppercase, got %q", value)
	}
	return value, nil
}

func validTokenTotals(e Event) bool {
	components := []*int64{e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheWriteTokens, e.ReasoningTokens}
	if e.TotalTokens == nil {
		for _, part := range components {
			if part != nil && *part < 0 {
				return false
			}
		}
		return true
	}
	total := *e.TotalTokens
	for _, part := range components {
		if part == nil {
			continue
		}
		if *part < 0 || *part > total {
			return false
		}
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
