// Package codex reads metadata-only token counters from Codex session logs.
package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tokemon/tokemon/internal/adapters"
	"github.com/tokemon/tokemon/internal/usage"
)

const (
	adapterID      = "codex"
	adapterVersion = "0.3.0"
)

type Adapter struct {
	root string
}

func New(home string) *Adapter {
	return &Adapter{root: filepath.Join(home, ".codex")}
}

func (a *Adapter) ID() string { return adapterID }

func (a *Adapter) NormalizeModel(raw string) string { return strings.TrimSpace(raw) }

func (a *Adapter) Capabilities() adapters.Capabilities {
	return adapters.Capabilities{InputTokens: true, OutputTokens: true, CacheTokens: true, ReasoningTokens: true, SessionID: true, Duration: true}
}

func (a *Adapter) Discover(ctx context.Context) ([]adapters.Source, error) {
	logs, err := a.discoverLogs(ctx)
	if err != nil {
		return nil, err
	}
	if len(logs) > 0 {
		return logs, nil
	}

	paths, err := filepath.Glob(filepath.Join(a.root, "state_*.sqlite"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var sources []adapters.Source
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		supported, err := supportsThreads(ctx, path)
		if err != nil || !supported {
			continue
		}
		sources = append(sources, adapters.Source{Path: path})
	}
	return sources, nil
}

func (a *Adapter) discoverLogs(ctx context.Context) ([]adapters.Source, error) {
	var sources []adapters.Source
	for _, root := range []string{filepath.Join(a.root, "sessions"), filepath.Join(a.root, "archived_sessions")} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if !entry.IsDir() && strings.EqualFold(filepath.Ext(path), ".jsonl") {
				sources = append(sources, adapters.Source{Path: path, Identity: adapters.HashIdentity(path)})
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })
	return sources, nil
}

func supportsThreads(ctx context.Context, path string) (bool, error) {
	db, err := adapters.OpenReadOnly(ctx, path)
	if err != nil {
		return false, err
	}
	defer db.Close()
	return adapters.HasTable(ctx, db, "threads")
}

func (a *Adapter) Parse(ctx context.Context, source adapters.Source, request adapters.ParseRequest) (adapters.ParseResult, error) {
	if strings.TrimSpace(request.MachineID) == "" {
		return adapters.ParseResult{}, fmt.Errorf("machine ID is required")
	}
	if strings.EqualFold(filepath.Ext(source.Path), ".jsonl") {
		return a.parseLog(ctx, source, request)
	}
	return a.parseThreadSnapshots(ctx, source, request)
}

type logRecord struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

type sessionMetadata struct {
	ID            string `json:"id"`
	SessionID     string `json:"session_id"`
	ModelProvider string `json:"model_provider"`
	CWD           string `json:"cwd"`
}

type turnContext struct {
	Model string `json:"model"`
	CWD   string `json:"cwd"`
}

type eventPayload struct {
	Type string     `json:"type"`
	Info *tokenInfo `json:"info"`
}

type tokenInfo struct {
	Model          string      `json:"model"`
	ModelName      string      `json:"model_name"`
	LastTokenUsage *tokenUsage `json:"last_token_usage"`
}

type tokenUsage struct {
	InputTokens          int64 `json:"input_tokens"`
	CachedInputTokens    int64 `json:"cached_input_tokens"`
	CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
	OutputTokens         int64 `json:"output_tokens"`
	ReasoningTokens      int64 `json:"reasoning_output_tokens"`
}

func (a *Adapter) parseLog(ctx context.Context, source adapters.Source, request adapters.ParseRequest) (adapters.ParseResult, error) {
	file, err := os.Open(source.Path)
	if err != nil {
		return adapters.ParseResult{}, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	provider, model, sessionID, project := "openai", "unknown", "", ""
	var events []usage.Event
	for lineNumber := int64(1); scanner.Scan(); lineNumber++ {
		if err := ctx.Err(); err != nil {
			return adapters.ParseResult{}, err
		}
		line := scanner.Bytes()
		if !bytes.Contains(line, []byte(`"session_meta"`)) && !bytes.Contains(line, []byte(`"turn_context"`)) && !bytes.Contains(line, []byte(`"token_count"`)) {
			continue
		}
		var record logRecord
		if err := json.Unmarshal(line, &record); err != nil {
			continue
		}
		switch record.Type {
		case "session_meta":
			var metadata sessionMetadata
			if json.Unmarshal(record.Payload, &metadata) != nil {
				continue
			}
			sessionID = firstNonEmpty(metadata.SessionID, metadata.ID, sessionID)
			provider = firstNonEmpty(metadata.ModelProvider, provider)
			project = firstNonEmpty(usage.NormalizeProject(metadata.CWD), project)
		case "turn_context":
			var turn turnContext
			if json.Unmarshal(record.Payload, &turn) != nil {
				continue
			}
			model = firstNonEmpty(a.NormalizeModel(turn.Model), model)
			project = firstNonEmpty(usage.NormalizeProject(turn.CWD), project)
		case "event_msg":
			var payload eventPayload
			if json.Unmarshal(record.Payload, &payload) != nil || payload.Type != "token_count" || payload.Info == nil || payload.Info.LastTokenUsage == nil {
				continue
			}
			tokens := payload.Info.LastTokenUsage
			cached := tokens.CachedInputTokens
			if cached == 0 {
				cached = tokens.CacheReadInputTokens
			}
			input := max(0, tokens.InputTokens)
			cached = min(max(0, cached), input)
			uncached := input - cached
			output := max(0, tokens.OutputTokens)
			reasoning := min(max(0, tokens.ReasoningTokens), output)
			if uncached == 0 && cached == 0 && output == 0 {
				continue
			}
			timestamp, err := time.Parse(time.RFC3339Nano, record.Timestamp)
			if err != nil {
				continue
			}
			currentModel := firstNonEmpty(a.NormalizeModel(payload.Info.Model), a.NormalizeModel(payload.Info.ModelName), model, "unknown")
			stableSessionID := sessionID
			if stableSessionID == "" {
				stableSessionID = strings.TrimSuffix(filepath.Base(source.Path), filepath.Ext(source.Path))
			}
			identity := adapters.HashIdentity(adapterID + ":session:" + stableSessionID)
			events = append(events, usage.Event{
				SchemaVersion:    usage.SchemaVersion,
				EventID:          usage.DeterministicID(request.MachineID, adapterID, identity, lineNumber, record.Timestamp, stableSessionID),
				Timestamp:        timestamp,
				MachineID:        request.MachineID,
				SessionID:        stableSessionID,
				Project:          project,
				Provider:         provider,
				Model:            currentModel,
				Tool:             "codex",
				InputTokens:      usage.Int64(uncached),
				OutputTokens:     usage.Int64(output),
				CacheReadTokens:  usage.Int64(cached),
				CacheWriteTokens: usage.Int64(0),
				ReasoningTokens:  usage.Int64(reasoning),
				TotalTokens:      usage.Int64(input + output),
				Currency:         "USD",
				TokenAccuracy:    usage.AccuracyReported,
				Source:           usage.Source{Adapter: adapterID, AdapterVersion: adapterVersion, Identity: identity, Offset: lineNumber},
			})
		}
	}
	if err := scanner.Err(); err != nil {
		return adapters.ParseResult{}, err
	}
	return adapters.ParseResult{Events: events, Cursor: adapters.Cursor{Identity: adapters.HashIdentity(source.Path)}}, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func (a *Adapter) parseThreadSnapshots(ctx context.Context, source adapters.Source, request adapters.ParseRequest) (adapters.ParseResult, error) {
	db, err := adapters.OpenReadOnly(ctx, source.Path)
	if err != nil {
		return adapters.ParseResult{}, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `
SELECT id, created_at_ms, updated_at_ms, COALESCE(model_provider, ''), COALESCE(model, ''), COALESCE(cwd, ''), tokens_used
FROM threads
WHERE tokens_used > 0
ORDER BY created_at_ms, id`)
	if err != nil {
		return adapters.ParseResult{}, fmt.Errorf("read Codex threads: %w", err)
	}
	defer rows.Close()

	sourceIdentity := adapterID + ":" + filepath.Base(source.Path) + ":threads"
	var events []usage.Event
	for rows.Next() {
		var sessionID, provider, model, cwd string
		var createdAt, updatedAt, tokens int64
		if err := rows.Scan(&sessionID, &createdAt, &updatedAt, &provider, &model, &cwd, &tokens); err != nil {
			return adapters.ParseResult{}, err
		}
		provider = firstNonEmpty(provider, "unknown")
		model = firstNonEmpty(a.NormalizeModel(model), "unknown")
		timestamp := adapters.UnixTime(updatedAt)
		if updatedAt <= 0 {
			timestamp = adapters.UnixTime(createdAt)
		}
		events = append(events, usage.Event{
			SchemaVersion: usage.SchemaVersion, EventID: usage.DeterministicID(request.MachineID, adapterID, sourceIdentity, 0, "snapshot", sessionID),
			Timestamp: timestamp, MachineID: request.MachineID, SessionID: sessionID, Project: usage.NormalizeProject(cwd), Provider: provider, Model: model,
			Tool: "codex", TotalTokens: usage.Int64(tokens), DurationMS: adapters.DurationMS(createdAt, updatedAt), Currency: "USD",
			TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: adapterID, AdapterVersion: "0.2.0", Identity: sourceIdentity},
		})
	}
	return adapters.ParseResult{Events: events, Cursor: adapters.Cursor{Identity: sourceIdentity}}, rows.Err()
}

var _ adapters.Adapter = (*Adapter)(nil)
