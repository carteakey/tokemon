// Package opencode reads OpenCode's local session aggregates.
package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tokemon/tokemon/internal/adapters"
	"github.com/tokemon/tokemon/internal/usage"
)

const (
	adapterID      = "opencode"
	adapterVersion = "0.2.0"
)

type Adapter struct {
	paths []string
	cache *adapters.SnapshotCache
}

func New(home string) *Adapter {
	return &Adapter{paths: []string{
		filepath.Join(home, ".local", "share", "opencode", "opencode.db"),
		filepath.Join(home, "Library", "Application Support", "opencode", "opencode.db"),
	}, cache: adapters.NewSnapshotCache()}
}

func (a *Adapter) ID() string { return adapterID }

func (a *Adapter) NormalizeModel(raw string) string {
	model, _ := modelMetadata(raw)
	if model == "" {
		return "unknown"
	}
	return model
}

func (a *Adapter) Capabilities() adapters.Capabilities {
	return adapters.Capabilities{
		InputTokens: true, OutputTokens: true, CacheTokens: true,
		ReasoningTokens: true, Cost: true, SessionID: true, Duration: true,
	}
}

func (a *Adapter) Discover(ctx context.Context) ([]adapters.Source, error) {
	var sources []adapters.Source
	seenPaths := make(map[string]struct{})
	for _, path := range a.paths {
		if _, ok := seenPaths[path]; ok {
			continue
		}
		seenPaths[path] = struct{}{}
		info, err := os.Stat(path)
		if os.IsNotExist(err) || (err == nil && info.IsDir()) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sources = append(sources, adapters.Source{Path: path})
	}
	seen := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		seen[source.Path] = struct{}{}
	}
	a.cache.Prune(seen)
	return sources, nil
}

func (a *Adapter) Parse(ctx context.Context, source adapters.Source, request adapters.ParseRequest) (adapters.ParseResult, error) {
	machineID := request.MachineID
	if strings.TrimSpace(machineID) == "" {
		return adapters.ParseResult{}, fmt.Errorf("machine ID is required")
	}
	signature, err := adapters.Signature(source.Path)
	if err != nil {
		return adapters.ParseResult{}, err
	}
	sourceIdentity := adapterID + ":" + filepath.Base(source.Path) + ":session"
	if cached, err, ok := a.cache.Lookup(source.Path, signature, sourceIdentity, request.Cursor); ok {
		return cached, err
	}
	result, err := a.parseUncached(ctx, source, request, machineID, sourceIdentity)
	a.cache.Store(source.Path, signature, sourceIdentity, request.Cursor, result, err)
	return result, err
}

func (a *Adapter) parseUncached(ctx context.Context, source adapters.Source, request adapters.ParseRequest, machineID, sourceIdentity string) (adapters.ParseResult, error) {
	db, err := adapters.OpenReadOnly(ctx, source.Path)
	if err != nil {
		return adapters.ParseResult{}, err
	}
	defer db.Close()
	hasSession, err := adapters.HasTable(ctx, db, "session")
	if err != nil {
		return adapters.ParseResult{}, err
	}
	if !hasSession {
		return adapters.ParseResult{Cursor: adapters.Cursor{Identity: sourceIdentity}}, nil
	}
	projectExpression := `''`
	if hasDirectory, err := adapters.HasColumn(ctx, db, "session", "directory"); err != nil {
		return adapters.ParseResult{}, err
	} else if hasDirectory {
		projectExpression = "directory"
	}
	rows, err := db.QueryContext(ctx, `
SELECT id, time_created, time_updated, model,
       tokens_input, tokens_output, tokens_reasoning,
       tokens_cache_read, tokens_cache_write, cost, `+projectExpression+`
FROM session
WHERE tokens_input + tokens_output + tokens_reasoning + tokens_cache_read + tokens_cache_write > 0
ORDER BY time_created, id`)
	if err != nil {
		return adapters.ParseResult{}, fmt.Errorf("read OpenCode sessions: %w", err)
	}
	defer rows.Close()

	var events []usage.Event
	for rows.Next() {
		var (
			sessionID, rawModel, workingDirectory           string
			createdAt, updatedAt                            int64
			input, output, reasoning, cacheRead, cacheWrite int64
			cost                                            float64
		)
		if err := rows.Scan(&sessionID, &createdAt, &updatedAt, &rawModel, &input, &output, &reasoning, &cacheRead, &cacheWrite, &cost, &workingDirectory); err != nil {
			return adapters.ParseResult{}, err
		}
		model, provider := modelMetadata(rawModel)
		if model == "" {
			model = "unknown"
		}
		if provider == "" {
			provider = "opencode"
		}
		total := input + output + reasoning + cacheRead + cacheWrite
		timestamp := adapters.UnixTime(updatedAt)
		if updatedAt <= 0 {
			timestamp = adapters.UnixTime(createdAt)
		}
		event := usage.Event{
			SchemaVersion:    usage.SchemaVersion,
			EventID:          usage.DeterministicID(machineID, adapterID, sourceIdentity, 0, "snapshot", sessionID),
			Timestamp:        timestamp,
			MachineID:        machineID,
			SessionID:        sessionID,
			Project:          usage.NormalizeProject(workingDirectory),
			Provider:         provider,
			Model:            model,
			Tool:             "opencode",
			InputTokens:      usage.Int64(input),
			OutputTokens:     usage.Int64(output),
			ReasoningTokens:  usage.Int64(reasoning),
			CacheReadTokens:  usage.Int64(cacheRead),
			CacheWriteTokens: usage.Int64(cacheWrite),
			TotalTokens:      usage.Int64(total),
			DurationMS:       adapters.DurationMS(createdAt, updatedAt),
			Currency:         "USD",
			TokenAccuracy:    usage.AccuracyDerived,
			Source:           usage.Source{Adapter: adapterID, AdapterVersion: adapterVersion, Identity: sourceIdentity},
		}
		if cost > 0 {
			event.Cost = usage.Float64(cost)
			event.CostEstimated = true
		}
		events = append(events, event)
	}
	return adapters.ParseResult{Events: events, Cursor: adapters.Cursor{Identity: sourceIdentity}}, rows.Err()
}

type modelRef struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
}

func modelMetadata(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	var ref modelRef
	if err := json.Unmarshal([]byte(raw), &ref); err == nil {
		return strings.TrimSpace(ref.ID), strings.TrimSpace(ref.ProviderID)
	}
	return raw, ""
}

var _ adapters.Adapter = (*Adapter)(nil)
