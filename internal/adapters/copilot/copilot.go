// Package copilot reads metadata-only usage aggregates from GitHub Copilot CLI
// session logs.
package copilot

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tokemon/tokemon/internal/adapters"
	"github.com/tokemon/tokemon/internal/usage"
)

const (
	adapterID      = "copilot-cli"
	adapterVersion = "0.1.0"
)

// Adapter reads the durable session event streams written by Copilot CLI.
// It deliberately consumes only session lifecycle metadata and shutdown
// aggregates; prompts, responses, tool arguments, and file paths are ignored.
type Adapter struct {
	root  string
	cache *adapters.SnapshotCache
}

func New(home string) *Adapter {
	return &Adapter{root: filepath.Join(home, ".copilot", "session-state"), cache: adapters.NewSnapshotCache()}
}

func (a *Adapter) ID() string { return adapterID }

func (a *Adapter) NormalizeModel(raw string) string {
	if model := strings.TrimSpace(raw); model != "" {
		return model
	}
	return "unknown"
}

func (a *Adapter) Capabilities() adapters.Capabilities {
	return adapters.Capabilities{
		InputTokens: true, OutputTokens: true, CacheTokens: true,
		ReasoningTokens: true, SessionID: true,
	}
}

func (a *Adapter) Discover(ctx context.Context) ([]adapters.Source, error) {
	paths, err := filepath.Glob(filepath.Join(a.root, "*", "events.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	sources := make([]adapters.Source, 0, len(paths))
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		if info.IsDir() {
			continue
		}
		sources = append(sources, adapters.Source{Path: path, Identity: sourceIdentity(path)})
	}
	seen := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		seen[source.Path] = struct{}{}
	}
	a.cache.Prune(seen)
	return sources, nil
}

func (a *Adapter) Parse(ctx context.Context, source adapters.Source, request adapters.ParseRequest) (adapters.ParseResult, error) {
	if strings.TrimSpace(request.MachineID) == "" {
		return adapters.ParseResult{}, errors.New("machine ID is required")
	}
	signature, err := adapters.Signature(source.Path)
	if err != nil {
		return adapters.ParseResult{}, err
	}
	identity := source.Identity
	if identity == "" {
		identity = sourceIdentity(source.Path)
	}
	if cached, err, ok := a.cache.Lookup(source.Path, signature, identity, request.Cursor); ok {
		return cached, err
	}
	result, err := a.parseUncached(ctx, source, request, identity)
	a.cache.Store(source.Path, signature, identity, request.Cursor, result, err)
	return result, err
}

func (a *Adapter) parseUncached(ctx context.Context, source adapters.Source, request adapters.ParseRequest, identity string) (adapters.ParseResult, error) {
	file, err := os.Open(source.Path)
	if err != nil {
		return adapters.ParseResult{}, err
	}
	defer file.Close()

	sessionID := filepath.Base(filepath.Dir(source.Path))
	var project string
	var sessionStart time.Time
	var latest *shutdownRecord
	var position int64
	reader := bufio.NewReaderSize(file, 64*1024)

	for {
		if err := ctx.Err(); err != nil {
			return adapters.ParseResult{}, err
		}
		lineOffset := position
		line, readErr := reader.ReadBytes('\n')
		if len(line) == 0 && errors.Is(readErr, io.EOF) {
			break
		}
		if errors.Is(readErr, io.EOF) && !bytes.HasSuffix(line, []byte{'\n'}) {
			// Copilot can be writing the final event while the agent polls. Do not
			// consume a line until its newline proves the JSON record is complete.
			position = lineOffset
			break
		}
		position += int64(len(line))
		if len(bytes.TrimSpace(line)) == 0 {
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return adapters.ParseResult{}, readErr
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			continue
		}

		var event sessionEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return adapters.ParseResult{}, fmt.Errorf("%s byte %d: %w", filepath.Base(source.Path), lineOffset, err)
		}
		switch event.Type {
		case "session.start":
			var data sessionStartData
			if err := json.Unmarshal(event.Data, &data); err == nil {
				if data.SessionID != "" {
					sessionID = data.SessionID
				}
				project = firstNonEmpty(project, usage.NormalizeProject(data.Context.CWD))
				if parsed, err := time.Parse(time.RFC3339Nano, data.StartTime); err == nil {
					sessionStart = parsed
				}
			}
		case "session.context_changed":
			var data contextChangedData
			if err := json.Unmarshal(event.Data, &data); err == nil {
				if normalized := usage.NormalizeProject(data.CWD); normalized != "" {
					project = normalized
				}
			}
		case "session.shutdown":
			var data shutdownData
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return adapters.ParseResult{}, fmt.Errorf("%s byte %d shutdown data: %w", filepath.Base(source.Path), lineOffset, err)
			}
			if len(data.ModelMetrics) > 0 {
				shutdownTime, _ := time.Parse(time.RFC3339Nano, event.Timestamp)
				latest = &shutdownRecord{data: data, timestamp: shutdownTime, offset: lineOffset}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return adapters.ParseResult{}, readErr
		}
	}

	result := adapters.ParseResult{Cursor: adapters.Cursor{Identity: identity, Offset: position}}
	if latest == nil {
		return result, nil
	}
	timestamp := latest.timestamp
	if latest.data.SessionStartTime > 0 {
		timestamp = time.UnixMilli(latest.data.SessionStartTime).UTC()
	} else if timestamp.IsZero() {
		timestamp = sessionStart
	}
	if timestamp.IsZero() {
		return result, nil
	}

	models := make([]string, 0, len(latest.data.ModelMetrics))
	for model := range latest.data.ModelMetrics {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, rawModel := range models {
		event, ok := normalizeMetric(latest.data.ModelMetrics[rawModel], request.MachineID, identity, sessionID, project, rawModel, timestamp, latest.offset)
		if ok {
			result.Events = append(result.Events, event)
		}
	}
	return result, nil
}

type sessionEvent struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

type sessionStartData struct {
	SessionID string `json:"sessionId"`
	StartTime string `json:"startTime"`
	Context   struct {
		CWD string `json:"cwd"`
	} `json:"context"`
}

type contextChangedData struct {
	CWD string `json:"cwd"`
}

type shutdownData struct {
	SessionStartTime int64                  `json:"sessionStartTime"`
	ModelMetrics     map[string]modelMetric `json:"modelMetrics"`
}

type shutdownRecord struct {
	data      shutdownData
	timestamp time.Time
	offset    int64
}

type modelMetric struct {
	Usage        *tokenUsage            `json:"usage"`
	Requests     *requestMetrics        `json:"requests"`
	TokenDetails map[string]tokenDetail `json:"tokenDetails"`
	InputTokens  *int64                 `json:"inputTokens"`
	OutputTokens *int64                 `json:"outputTokens"`
	CacheRead    *int64                 `json:"cacheReadTokens"`
	CacheWrite   *int64                 `json:"cacheWriteTokens"`
	Reasoning    *int64                 `json:"reasoningTokens"`
}

type requestMetrics struct {
	Count *int64   `json:"count"`
	Cost  *float64 `json:"cost"`
}

type tokenDetail struct {
	TokenCount *int64 `json:"tokenCount"`
}

type tokenUsage struct {
	InputTokens  *int64 `json:"inputTokens"`
	OutputTokens *int64 `json:"outputTokens"`
	CacheRead    *int64 `json:"cacheReadTokens"`
	CacheWrite   *int64 `json:"cacheWriteTokens"`
	Reasoning    *int64 `json:"reasoningTokens"`
}

func (m modelMetric) tokens() tokenUsage {
	if m.Usage != nil {
		return *m.Usage
	}
	tokens := tokenUsage{
		InputTokens: m.InputTokens, OutputTokens: m.OutputTokens,
		CacheRead: m.CacheRead, CacheWrite: m.CacheWrite, Reasoning: m.Reasoning,
	}
	for name, detail := range m.TokenDetails {
		if detail.TokenCount == nil {
			continue
		}
		switch strings.ToLower(strings.ReplaceAll(name, "_", "")) {
		case "input", "inputtokens":
			tokens.InputTokens = detail.TokenCount
		case "output", "outputtokens":
			tokens.OutputTokens = detail.TokenCount
		case "cacheread", "cachereadtokens":
			tokens.CacheRead = detail.TokenCount
		case "cachewrite", "cachewritetokens":
			tokens.CacheWrite = detail.TokenCount
		case "reasoning", "reasoningtokens":
			tokens.Reasoning = detail.TokenCount
		}
	}
	return tokens
}

func normalizeMetric(metric modelMetric, machineID, identity, sessionID, project, rawModel string, timestamp time.Time, offset int64) (usage.Event, bool) {
	tokens := metric.tokens()
	input := safeToken(tokens.InputTokens)
	output := safeToken(tokens.OutputTokens)
	cacheRead := safeToken(tokens.CacheRead)
	cacheWrite := safeToken(tokens.CacheWrite)
	reasoning := safeToken(tokens.Reasoning)
	if input == nil && output == nil && cacheRead == nil && cacheWrite == nil && reasoning == nil {
		return usage.Event{}, false
	}
	model := firstNonEmpty(rawModel, "unknown")

	var total *int64
	accuracy := usage.AccuracyUnknown
	if input != nil && output != nil && cacheRead != nil && cacheWrite != nil {
		sum := *input + *output + *cacheRead + *cacheWrite
		if reasoning != nil {
			sum += *reasoning
		}
		total = usage.Int64(sum)
		accuracy = usage.AccuracyDerived
	}
	return usage.Event{
		SchemaVersion:    usage.SchemaVersion,
		EventID:          usage.DeterministicID(machineID, adapterID, identity, 0, model, sessionID),
		Timestamp:        timestamp,
		MachineID:        machineID,
		SessionID:        sessionID,
		Project:          project,
		Provider:         "github",
		Model:            model,
		Tool:             adapterID,
		InputTokens:      input,
		OutputTokens:     output,
		CacheReadTokens:  cacheRead,
		CacheWriteTokens: cacheWrite,
		ReasoningTokens:  reasoning,
		TotalTokens:      total,
		Currency:         "USD",
		TokenAccuracy:    accuracy,
		Source:           usage.Source{Adapter: adapterID, AdapterVersion: adapterVersion, Identity: identity, Offset: offset},
	}, true
}

func safeToken(value *int64) *int64 {
	if value == nil || *value < 0 {
		return nil
	}
	return usage.Int64(*value)
}

func sourceIdentity(path string) string {
	return adapters.HashIdentity(adapterID + ":" + filepath.Clean(path))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

var _ adapters.Adapter = (*Adapter)(nil)
