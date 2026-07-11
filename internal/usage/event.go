package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const SchemaVersion = "1"

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
	if strings.TrimSpace(e.EventID) == "" {
		return errors.New("event_id is required")
	}
	if e.Timestamp.IsZero() {
		return errors.New("timestamp is required")
	}
	if strings.TrimSpace(e.MachineID) == "" {
		return errors.New("machine_id is required")
	}
	if strings.TrimSpace(e.Provider) == "" {
		return errors.New("provider is required")
	}
	if strings.TrimSpace(e.Model) == "" {
		return errors.New("model is required")
	}
	if strings.TrimSpace(e.Tool) == "" {
		return errors.New("tool is required")
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
	if e.Cost != nil && *e.Cost < 0 {
		return errors.New("cost cannot be negative")
	}
	return nil
}

func DeterministicID(machineID, adapter, sourceIdentity string, offset int64, timestamp, sessionID string) string {
	input := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s\x00%s", machineID, adapter, sourceIdentity, offset, timestamp, sessionID)
	hash := sha256.Sum256([]byte(input))
	return "sha256:" + hex.EncodeToString(hash[:])
}

func Int64(value int64) *int64 { return &value }

func Float64(value float64) *float64 { return &value }
