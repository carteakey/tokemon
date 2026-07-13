// Package claude reads Claude Code session transcript usage metadata.
package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tokemon/tokemon/internal/adapters"
	"github.com/tokemon/tokemon/internal/usage"
)

const (
	adapterID      = "claude-code"
	adapterVersion = "0.2.0"
)

type Adapter struct {
	root string
}

func New(home string) *Adapter {
	return &Adapter{root: filepath.Join(home, ".claude", "projects")}
}

func (a *Adapter) ID() string { return adapterID }

func (a *Adapter) NormalizeModel(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "unknown"
	}
	return raw
}

func (a *Adapter) Capabilities() adapters.Capabilities {
	return adapters.Capabilities{
		InputTokens: true, OutputTokens: true, CacheTokens: true,
		SessionID: true,
	}
}

func (a *Adapter) Discover(ctx context.Context) ([]adapters.Source, error) {
	if _, err := os.Stat(a.root); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var paths []string
	err := filepath.WalkDir(a.root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type().IsRegular() && strings.EqualFold(filepath.Ext(entry.Name()), ".jsonl") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	sources := make([]adapters.Source, 0, len(paths))
	for _, path := range paths {
		sources = append(sources, adapters.Source{Path: path})
	}
	return sources, nil
}

func (a *Adapter) Parse(ctx context.Context, source adapters.Source, request adapters.ParseRequest) (adapters.ParseResult, error) {
	machineID := request.MachineID
	if strings.TrimSpace(machineID) == "" {
		return adapters.ParseResult{}, fmt.Errorf("machine ID is required")
	}
	file, err := os.Open(source.Path)
	if err != nil {
		return adapters.ParseResult{}, err
	}
	defer file.Close()

	identity := a.sourceIdentity(source.Path)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	var events []usage.Event
	for lineNumber := int64(1); scanner.Scan(); lineNumber++ {
		if err := ctx.Err(); err != nil {
			return adapters.ParseResult{}, err
		}
		var record transcriptRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			// Claude transcripts can contain partially written final records while
			// a session is active. The next poll will see the complete record.
			continue
		}
		if record.Type == "system" && record.Subtype == "turn_duration" && record.DurationMS != nil {
			// Claude Code writes turn duration as a metadata-only system record
			// immediately after the assistant response it describes. Keep it on
			// that usage event instead of emitting a second, tokenless event.
			if len(events) > 0 && (record.SessionID == "" || events[len(events)-1].SessionID == record.SessionID) {
				events[len(events)-1].DurationMS = record.DurationMS
			}
			continue
		}
		if record.Type != "assistant" || record.Message == nil {
			continue
		}
		rawUsage := record.Message.Usage
		if rawUsage == nil {
			rawUsage = record.Usage
		}
		if rawUsage == nil || !rawUsage.HasTokens() {
			continue
		}
		timestamp, err := parseTimestamp(record.Timestamp)
		if err != nil {
			continue
		}
		sessionID := record.SessionID
		if sessionID == "" {
			sessionID = record.SessionIDAlt
		}
		if sessionID == "" {
			sessionID = strings.TrimSuffix(filepath.Base(source.Path), filepath.Ext(source.Path))
		}
		event := normalize(record, rawUsage, machineID, identity, lineNumber, sessionID, timestamp)
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return adapters.ParseResult{}, err
	}
	return adapters.ParseResult{Events: events}, nil
}

func (a *Adapter) sourceIdentity(path string) string {
	relative, err := filepath.Rel(a.root, path)
	if err != nil {
		relative = filepath.Base(path)
	}
	return adapters.HashIdentity(adapterID + ":" + filepath.ToSlash(relative))
}

type transcriptRecord struct {
	Type         string         `json:"type"`
	Subtype      string         `json:"subtype"`
	Timestamp    string         `json:"timestamp"`
	SessionID    string         `json:"sessionId"`
	SessionIDAlt string         `json:"session_id"`
	DurationMS   *int64         `json:"durationMs"`
	Message      *messageRecord `json:"message"`
	Usage        *tokenUsage    `json:"usage"`
}

type messageRecord struct {
	Model string      `json:"model"`
	Usage *tokenUsage `json:"usage"`
}

type tokenUsage struct {
	InputTokens      *int64         `json:"input_tokens"`
	OutputTokens     *int64         `json:"output_tokens"`
	CacheReadTokens  *int64         `json:"cache_read_input_tokens"`
	CacheWriteTokens *int64         `json:"cache_creation_input_tokens"`
	TotalTokens      *int64         `json:"total_tokens"`
	CacheCreation    *cacheCreation `json:"cache_creation"`
}

type cacheCreation struct {
	Ephemeral5mInputTokens *int64 `json:"ephemeral_5m_input_tokens"`
	Ephemeral1hInputTokens *int64 `json:"ephemeral_1h_input_tokens"`
}

func (u *tokenUsage) HasTokens() bool {
	return u.InputTokens != nil || u.OutputTokens != nil || u.CacheReadTokens != nil || u.CacheWriteTokens != nil || u.TotalTokens != nil || u.CacheCreation != nil
}

func normalize(record transcriptRecord, raw *tokenUsage, machineID, identity string, lineNumber int64, sessionID string, timestamp time.Time) usage.Event {
	cacheWrite := raw.CacheWriteTokens
	if cacheWrite == nil && raw.CacheCreation != nil {
		cacheWrite = sumPointers(raw.CacheCreation.Ephemeral5mInputTokens, raw.CacheCreation.Ephemeral1hInputTokens)
	}

	total := raw.TotalTokens
	accuracy := usage.AccuracyReported
	if total == nil && raw.InputTokens != nil && raw.OutputTokens != nil && raw.CacheReadTokens != nil && cacheWrite != nil {
		total = sumPointers(raw.InputTokens, raw.OutputTokens, raw.CacheReadTokens, cacheWrite)
		accuracy = usage.AccuracyDerived
	}
	if total == nil {
		accuracy = usage.AccuracyUnknown
	}

	model := record.Message.Model
	if model == "" {
		model = "unknown"
	}
	return usage.Event{
		SchemaVersion:    usage.SchemaVersion,
		EventID:          usage.DeterministicID(machineID, adapterID, identity, lineNumber, timestamp.UTC().Format(time.RFC3339Nano), sessionID),
		Timestamp:        timestamp,
		MachineID:        machineID,
		SessionID:        sessionID,
		Provider:         "anthropic",
		Model:            model,
		Tool:             "claude-code",
		InputTokens:      raw.InputTokens,
		OutputTokens:     raw.OutputTokens,
		CacheReadTokens:  raw.CacheReadTokens,
		CacheWriteTokens: cacheWrite,
		TotalTokens:      total,
		Currency:         "USD",
		TokenAccuracy:    accuracy,
		Source:           usage.Source{Adapter: adapterID, AdapterVersion: adapterVersion, Identity: identity, Offset: lineNumber},
	}
}

func sumPointers(values ...*int64) *int64 {
	var total int64
	for _, value := range values {
		if value == nil {
			return nil
		}
		total += *value
	}
	return usage.Int64(total)
}

func parseTimestamp(value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, fmt.Errorf("timestamp is required")
	}
	return time.Parse(time.RFC3339Nano, value)
}

var _ adapters.Adapter = (*Adapter)(nil)
