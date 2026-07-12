// Package opencode reads OpenCode's local session aggregates.
package opencode

import (
	"context"
	"encoding/json"
	"fmt"
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
}

func New(home string) *Adapter {
	return &Adapter{paths: []string{
		filepath.Join(home, ".local", "share", "opencode", "opencode.db"),
		filepath.Join(home, "Library", "Application Support", "opencode", "opencode.db"),
	}}
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
	seen := make(map[string]struct{})
	for _, path := range a.paths {
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		supported, err := supportsSessions(ctx, path)
		if err != nil || !supported {
			continue
		}
		sources = append(sources, adapters.Source{Path: path})
	}
	return sources, nil
}

func supportsSessions(ctx context.Context, path string) (bool, error) {
	db, err := adapters.OpenReadOnly(ctx, path)
	if err != nil {
		return false, err
	}
	defer db.Close()
	return adapters.HasTable(ctx, db, "session")
}

func (a *Adapter) Parse(ctx context.Context, source adapters.Source, request adapters.ParseRequest) (adapters.ParseResult, error) {
	machineID := request.MachineID
	if strings.TrimSpace(machineID) == "" {
		return adapters.ParseResult{}, fmt.Errorf("machine ID is required")
	}
	db, err := adapters.OpenReadOnly(ctx, source.Path)
	if err != nil {
		return adapters.ParseResult{}, err
	}
	defer db.Close()
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

	sourceIdentity := adapterID + ":" + filepath.Base(source.Path) + ":session"
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
	return adapters.ParseResult{Events: events}, rows.Err()
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
