// Package codex reads metadata-only token counters from Codex session logs.
package codex

import (
	"bufio"
	"bytes"
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
	adapterID      = "codex"
	adapterVersion = "0.6.0"
)

type Adapter struct {
	root           string
	cache          *adapters.SnapshotCache
	directories    *adapters.DirectoryCache
	sessionFiles   map[string]string
	metadataByPath map[string]sessionMetadata
}

func New(home string) *Adapter {
	return &Adapter{root: filepath.Join(home, ".codex"), cache: adapters.NewSnapshotCache(), directories: adapters.NewDirectoryCache()}
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
		if err := a.refreshSessionIndex(ctx, logs); err != nil {
			return nil, err
		}
		seen := make(map[string]struct{}, len(logs))
		for _, source := range logs {
			seen[source.Path] = struct{}{}
		}
		a.cache.Prune(seen)
		return logs, nil
	}

	paths, err := filepath.Glob(filepath.Join(a.root, "state_*.sqlite"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	sources := make([]adapters.Source, 0, len(paths))
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sources = append(sources, adapters.Source{Path: path})
	}
	if err := a.refreshSessionIndex(ctx, nil); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		seen[source.Path] = struct{}{}
	}
	a.cache.Prune(seen)
	return sources, nil
}

func (a *Adapter) discoverLogs(ctx context.Context) ([]adapters.Source, error) {
	var sources []adapters.Source
	for _, root := range []string{filepath.Join(a.root, "sessions"), filepath.Join(a.root, "archived_sessions")} {
		paths, err := a.directories.Paths(ctx, root, ".jsonl")
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		for _, path := range paths {
			sources = append(sources, adapters.Source{Path: path, Identity: adapters.HashIdentity(path)})
		}
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })
	return sources, nil
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
	ID                 string `json:"id"`
	SessionID          string `json:"session_id"`
	ModelProvider      string `json:"model_provider"`
	CWD                string `json:"cwd"`
	ForkedFromID       string `json:"forked_from_id"`
	ForkedFromIDAlt    string `json:"forkedFromId"`
	ParentSessionID    string `json:"parent_session_id"`
	ParentSessionIDAlt string `json:"parentSessionId"`
	Timestamp          string `json:"timestamp"`
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
	Model           string      `json:"model"`
	ModelName       string      `json:"model_name"`
	LastTokenUsage  *tokenUsage `json:"last_token_usage"`
	TotalTokenUsage *tokenUsage `json:"total_token_usage"`
}

type tokenUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheReadInputTokens  int64 `json:"cache_read_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningTokens       int64 `json:"reasoning_output_tokens"`
	TotalTokens           int64 `json:"total_tokens"`
}

type normalizedTokenUsage struct {
	InputTokens       int64
	CachedInputTokens int64
	CacheWriteTokens  int64
	OutputTokens      int64
	ReasoningTokens   int64
	TotalTokens       int64
}

func normalizeTokenUsage(tokens *tokenUsage) normalizedTokenUsage {
	if tokens == nil {
		return normalizedTokenUsage{}
	}
	input := max(0, tokens.InputTokens)
	cached := max(0, tokens.CachedInputTokens)
	if cached == 0 {
		cached = max(0, tokens.CacheReadInputTokens)
	}
	cached = min(cached, input)
	cacheWrite := max(0, tokens.CacheWriteInputTokens)
	output := max(0, tokens.OutputTokens)
	reasoning := min(max(0, tokens.ReasoningTokens), output)
	total := max(0, tokens.TotalTokens)
	if total == 0 && (input > 0 || output > 0) {
		total = input + output
	}
	return normalizedTokenUsage{
		InputTokens:       input,
		CachedInputTokens: cached,
		CacheWriteTokens:  cacheWrite,
		OutputTokens:      output,
		ReasoningTokens:   reasoning,
		TotalTokens:       total,
	}
}

func (value normalizedTokenUsage) delta(previous normalizedTokenUsage) normalizedTokenUsage {
	input := max(0, value.InputTokens-previous.InputTokens)
	cached := min(input, max(0, value.CachedInputTokens-previous.CachedInputTokens))
	output := max(0, value.OutputTokens-previous.OutputTokens)
	reasoning := min(output, max(0, value.ReasoningTokens-previous.ReasoningTokens))
	return normalizedTokenUsage{
		InputTokens:       input - cached,
		CachedInputTokens: cached,
		CacheWriteTokens:  max(0, value.CacheWriteTokens-previous.CacheWriteTokens),
		OutputTokens:      output,
		ReasoningTokens:   reasoning,
		TotalTokens:       max(0, value.TotalTokens-previous.TotalTokens),
	}
}

func (value normalizedTokenUsage) isZero() bool {
	return value.InputTokens == 0 && value.CachedInputTokens == 0 && value.CacheWriteTokens == 0 && value.OutputTokens == 0 && value.ReasoningTokens == 0 && value.TotalTokens == 0
}

func (metadata sessionMetadata) forkParentID() string {
	return firstNonEmpty(metadata.ForkedFromID, metadata.ForkedFromIDAlt, metadata.ParentSessionID, metadata.ParentSessionIDAlt)
}

func (metadata sessionMetadata) forkTimestamp(fallback string) string {
	return firstNonEmpty(metadata.Timestamp, fallback)
}

func eventTokenUsage(value normalizedTokenUsage) normalizedTokenUsage {
	cached := min(max(0, value.CachedInputTokens), max(0, value.InputTokens))
	return normalizedTokenUsage{
		InputTokens:       max(0, value.InputTokens) - cached,
		CachedInputTokens: cached,
		CacheWriteTokens:  max(0, value.CacheWriteTokens),
		OutputTokens:      max(0, value.OutputTokens),
		ReasoningTokens:   min(max(0, value.ReasoningTokens), max(0, value.OutputTokens)),
		TotalTokens:       max(0, value.TotalTokens),
	}
}

func subtractInheritedPrefix(value normalizedTokenUsage, remaining *normalizedTokenUsage) normalizedTokenUsage {
	adjusted := normalizedTokenUsage{
		InputTokens:       max(0, value.InputTokens-remaining.InputTokens),
		CachedInputTokens: max(0, value.CachedInputTokens-remaining.CachedInputTokens),
		CacheWriteTokens:  max(0, value.CacheWriteTokens-remaining.CacheWriteTokens),
		OutputTokens:      max(0, value.OutputTokens-remaining.OutputTokens),
		ReasoningTokens:   max(0, value.ReasoningTokens-remaining.ReasoningTokens),
		TotalTokens:       max(0, value.TotalTokens-remaining.TotalTokens),
	}
	remaining.InputTokens = max(0, remaining.InputTokens-value.InputTokens)
	remaining.CachedInputTokens = max(0, remaining.CachedInputTokens-value.CachedInputTokens)
	remaining.CacheWriteTokens = max(0, remaining.CacheWriteTokens-value.CacheWriteTokens)
	remaining.OutputTokens = max(0, remaining.OutputTokens-value.OutputTokens)
	remaining.ReasoningTokens = max(0, remaining.ReasoningTokens-value.ReasoningTokens)
	remaining.TotalTokens = max(0, remaining.TotalTokens-value.TotalTokens)
	return adjusted
}

func readSessionMetadata(ctx context.Context, path string) (sessionMetadata, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return sessionMetadata{}, false, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return sessionMetadata{}, false, err
		}
		line := scanner.Bytes()
		if !bytes.Contains(line, []byte(`"session_meta"`)) {
			continue
		}
		var record logRecord
		if err := json.Unmarshal(line, &record); err != nil || record.Type != "session_meta" {
			continue
		}
		var metadata sessionMetadata
		if err := json.Unmarshal(record.Payload, &metadata); err != nil {
			continue
		}
		metadata.Timestamp = metadata.forkTimestamp(record.Timestamp)
		return metadata, true, nil
	}
	return sessionMetadata{}, false, scanner.Err()
}

func (a *Adapter) refreshSessionIndex(ctx context.Context, sources []adapters.Source) error {
	files := make(map[string]string)
	metadataByPath := make(map[string]sessionMetadata)
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		metadata, ok, err := readSessionMetadata(ctx, source.Path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			continue
		}
		if !ok {
			continue
		}
		metadataByPath[source.Path] = metadata
		id := firstNonEmpty(metadata.ID, metadata.SessionID)
		if id != "" {
			if _, exists := files[id]; !exists {
				files[id] = source.Path
			}
		}
	}
	a.sessionFiles = files
	a.metadataByPath = metadataByPath
	return nil
}

func (a *Adapter) sourceMetadata(ctx context.Context, path string) (sessionMetadata, bool) {
	if metadata, ok := a.metadataByPath[path]; ok {
		return metadata, true
	}
	metadata, ok, err := readSessionMetadata(ctx, path)
	return metadata, ok && err == nil
}

func (a *Adapter) logCacheIdentity(ctx context.Context, source adapters.Source, identity string) string {
	metadata, ok := a.sourceMetadata(ctx, source.Path)
	if !ok {
		return identity
	}
	parentID := metadata.forkParentID()
	if parentID == "" {
		return identity
	}
	parentPath := a.sessionFiles[parentID]
	if parentPath == "" || parentPath == source.Path {
		return adapters.HashIdentity(identity + "\x00fork\x00" + parentID + "\x00unresolved")
	}
	parentSignature, err := adapters.Signature(parentPath)
	if err != nil {
		return adapters.HashIdentity(identity + "\x00fork\x00" + parentID + "\x00" + parentPath + "\x00unavailable")
	}
	return adapters.HashIdentity(fmt.Sprintf(
		"%s\x00fork\x00%s\x00%s\x00%d\x00%d\x00%d\x00%d\x00%t",
		identity,
		parentID,
		parentPath,
		parentSignature.Size,
		parentSignature.ModTime,
		parentSignature.SidecarSize,
		parentSignature.SidecarTime,
		parentSignature.Sidecar,
	))
}

func (a *Adapter) inheritedCumulativeUsage(ctx context.Context, path, forkTimestamp string) (normalizedTokenUsage, bool, error) {
	forkAt, err := time.Parse(time.RFC3339Nano, forkTimestamp)
	if err != nil {
		return normalizedTokenUsage{}, false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return normalizedTokenUsage{}, false, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	var latest normalizedTokenUsage
	found := false
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return normalizedTokenUsage{}, false, err
		}
		line := scanner.Bytes()
		if !bytes.Contains(line, []byte(`"token_count"`)) {
			continue
		}
		var record logRecord
		if err := json.Unmarshal(line, &record); err != nil || record.Type != "event_msg" {
			continue
		}
		var payload eventPayload
		if err := json.Unmarshal(record.Payload, &payload); err != nil || payload.Type != "token_count" || payload.Info == nil || payload.Info.TotalTokenUsage == nil {
			continue
		}
		timestamp, err := time.Parse(time.RFC3339Nano, record.Timestamp)
		if err != nil || timestamp.After(forkAt) {
			continue
		}
		latest = normalizeTokenUsage(payload.Info.TotalTokenUsage)
		found = true
	}
	if err := scanner.Err(); err != nil {
		return normalizedTokenUsage{}, false, err
	}
	return latest, found, nil
}

func (a *Adapter) resolveForkBaseline(ctx context.Context, sourcePath string, metadata sessionMetadata) (normalizedTokenUsage, bool) {
	parentID := metadata.forkParentID()
	forkTimestamp := metadata.forkTimestamp("")
	if parentID == "" || forkTimestamp == "" {
		return normalizedTokenUsage{}, false
	}
	parentPath := a.sessionFiles[parentID]
	if parentPath == "" && len(a.sessionFiles) == 0 {
		if sources, err := a.discoverLogs(ctx); err == nil {
			_ = a.refreshSessionIndex(ctx, sources)
			parentPath = a.sessionFiles[parentID]
		}
	}
	if parentPath == "" || parentPath == sourcePath {
		return normalizedTokenUsage{}, false
	}
	baseline, found, err := a.inheritedCumulativeUsage(ctx, parentPath, forkTimestamp)
	if err != nil || !found {
		return normalizedTokenUsage{}, false
	}
	return baseline, true
}

func (a *Adapter) parseLog(ctx context.Context, source adapters.Source, request adapters.ParseRequest) (adapters.ParseResult, error) {
	signature, err := adapters.Signature(source.Path)
	if err != nil {
		return adapters.ParseResult{}, err
	}
	identity := source.Identity
	if identity == "" {
		identity = adapters.HashIdentity(source.Path)
	}
	cacheIdentity := a.logCacheIdentity(ctx, source, identity)
	if cached, err, ok := a.cache.Lookup(source.Path, signature, cacheIdentity, request.Cursor); ok {
		return cached, err
	}
	result, err := a.parseLogUncached(ctx, source, request)
	a.cache.Store(source.Path, signature, cacheIdentity, request.Cursor, result, err)
	return result, err
}

func (a *Adapter) parseLogUncached(ctx context.Context, source adapters.Source, request adapters.ParseRequest) (adapters.ParseResult, error) {
	file, err := os.Open(source.Path)
	if err != nil {
		return adapters.ParseResult{}, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	provider, model, project := "openai", "unknown", ""
	var sessionID, sourceSessionID string
	var events []usage.Event
	seenUsage := make(map[string]struct{})
	metadata, _ := a.sourceMetadata(ctx, source.Path)
	forkBaseline, forkResolved := a.resolveForkBaseline(ctx, source.Path, metadata)
	remainingForkPrefix := forkBaseline
	previousCumulative := normalizedTokenUsage{}
	hasPreviousCumulative := false
	forkPrefixPending := forkResolved
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
			metadata.Timestamp = metadata.forkTimestamp(record.Timestamp)
			if !forkResolved {
				if baseline, resolved := a.resolveForkBaseline(ctx, source.Path, metadata); resolved {
					forkBaseline = baseline
					remainingForkPrefix = baseline
					previousCumulative = normalizedTokenUsage{}
					hasPreviousCumulative = false
					forkPrefixPending = true
					forkResolved = true
				}
			}
			// Forked logs expose the child thread in id and the original thread
			// in session_id. Count the child as its own chat, while retaining the
			// old parent-based identity for event IDs during the migration.
			sessionID = firstNonEmpty(sessionID, usage.NormalizeSessionID(metadata.ID), usage.NormalizeSessionID(metadata.SessionID))
			sourceSessionID = firstNonEmpty(sourceSessionID, usage.NormalizeSessionID(metadata.SessionID), usage.NormalizeSessionID(metadata.ID))
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
			if json.Unmarshal(record.Payload, &payload) != nil || payload.Type != "token_count" || payload.Info == nil || (payload.Info.LastTokenUsage == nil && payload.Info.TotalTokenUsage == nil) {
				continue
			}
			var tokenValues normalizedTokenUsage
			usingCumulative := payload.Info.TotalTokenUsage != nil
			if usingCumulative {
				current := normalizeTokenUsage(payload.Info.TotalTokenUsage)
				copiedCumulativePrefix := false
				if forkPrefixPending {
					// Codex fork logs can replay the parent from its beginning. Keep
					// suppressing copied cumulative snapshots until the child passes
					// the parent's fork-time baseline.
					if current.TotalTokens <= forkBaseline.TotalTokens {
						tokenValues = normalizedTokenUsage{}
						copiedCumulativePrefix = true
					} else {
						previousCumulative = forkBaseline
						hasPreviousCumulative = true
						forkPrefixPending = false
					}
				}
				if !copiedCumulativePrefix {
					if hasPreviousCumulative && current.TotalTokens < previousCumulative.TotalTokens {
						// A reset starts a new cumulative sequence. Count the first
						// snapshot in the new sequence instead of producing a negative
						// delta or silently losing the reset segment.
						previousCumulative = normalizedTokenUsage{}
					}
					hadPreviousCumulative := hasPreviousCumulative
					tokenValues = current.delta(previousCumulative)
					previousCumulative = current
					hasPreviousCumulative = true
					if tokenValues.isZero() && !hadPreviousCumulative {
						continue
					}
				}
			} else {
				raw := normalizeTokenUsage(payload.Info.LastTokenUsage)
				if raw.isZero() {
					continue
				}
				if forkResolved {
					raw = subtractInheritedPrefix(raw, &remainingForkPrefix)
				}
				tokenValues = eventTokenUsage(raw)
				if tokenValues.isZero() && !forkResolved {
					continue
				}
			}
			timestamp, err := time.Parse(time.RFC3339Nano, record.Timestamp)
			if err != nil {
				continue
			}
			currentModel := firstNonEmpty(a.NormalizeModel(payload.Info.Model), a.NormalizeModel(payload.Info.ModelName), model, "unknown")
			stableSessionID := firstNonEmpty(sessionID, usage.HashSessionID(source.Path))
			eventSessionID := firstNonEmpty(sourceSessionID, stableSessionID)
			identity := adapters.HashIdentity(adapterID + ":session:" + eventSessionID)
			if !usingCumulative {
				raw := normalizeTokenUsage(payload.Info.LastTokenUsage)
				usageKey := codexUsageKey(stableSessionID, timestamp.UTC().Format(time.RFC3339Nano), provider, currentModel, raw.InputTokens, raw.CachedInputTokens, raw.OutputTokens, raw.ReasoningTokens, raw.TotalTokens)
				if _, exists := seenUsage[usageKey]; exists {
					continue
				}
				seenUsage[usageKey] = struct{}{}
			}
			events = append(events, usage.Event{
				SchemaVersion:    usage.SchemaVersion,
				EventID:          usage.DeterministicID(request.MachineID, adapterID, identity, lineNumber, record.Timestamp, eventSessionID),
				Timestamp:        timestamp,
				MachineID:        request.MachineID,
				SessionID:        stableSessionID,
				Project:          project,
				Provider:         provider,
				Model:            currentModel,
				Tool:             "codex",
				InputTokens:      usage.Int64(tokenValues.InputTokens),
				OutputTokens:     usage.Int64(tokenValues.OutputTokens),
				CacheReadTokens:  usage.Int64(tokenValues.CachedInputTokens),
				CacheWriteTokens: usage.Int64(tokenValues.CacheWriteTokens),
				ReasoningTokens:  usage.Int64(tokenValues.ReasoningTokens),
				TotalTokens:      usage.Int64(tokenValues.TotalTokens),
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

func codexUsageKey(sessionID, timestamp, provider, model string, input, cached, output, reasoning, total int64) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d", sessionID, timestamp, provider, model, input, cached, 0, output, reasoning, total)
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
	signature, err := adapters.Signature(source.Path)
	if err != nil {
		return adapters.ParseResult{}, err
	}
	sourceIdentity := adapterID + ":" + filepath.Base(source.Path) + ":threads"
	if cached, err, ok := a.cache.Lookup(source.Path, signature, sourceIdentity, request.Cursor); ok {
		return cached, err
	}
	result, err := a.parseThreadSnapshotsUncached(ctx, source, request, sourceIdentity)
	a.cache.Store(source.Path, signature, sourceIdentity, request.Cursor, result, err)
	return result, err
}

func (a *Adapter) parseThreadSnapshotsUncached(ctx context.Context, source adapters.Source, request adapters.ParseRequest, sourceIdentity string) (adapters.ParseResult, error) {
	db, err := adapters.OpenReadOnly(ctx, source.Path)
	if err != nil {
		return adapters.ParseResult{}, err
	}
	defer db.Close()
	hasThreads, err := adapters.HasTable(ctx, db, "threads")
	if err != nil {
		return adapters.ParseResult{}, err
	}
	if !hasThreads {
		return adapters.ParseResult{Cursor: adapters.Cursor{Identity: sourceIdentity}}, nil
	}
	rows, err := db.QueryContext(ctx, `
SELECT id, created_at_ms, updated_at_ms, COALESCE(model_provider, ''), COALESCE(model, ''), COALESCE(cwd, ''), tokens_used
FROM threads
WHERE tokens_used > 0
ORDER BY created_at_ms, id`)
	if err != nil {
		return adapters.ParseResult{}, fmt.Errorf("read Codex threads: %w", err)
	}
	defer rows.Close()

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
