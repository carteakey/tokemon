// Package codex reads the aggregate thread metadata maintained by Codex.
package codex

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tokemon/tokemon/internal/adapters"
	"github.com/tokemon/tokemon/internal/usage"
)

const (
	adapterID      = "codex"
	adapterVersion = "0.2.0"
)

type Adapter struct {
	root string
}

func New(home string) *Adapter {
	return &Adapter{root: filepath.Join(home, ".codex")}
}

func (a *Adapter) ID() string { return adapterID }

func (a *Adapter) NormalizeModel(raw string) string {
	return strings.TrimSpace(raw)
}

func (a *Adapter) Capabilities() adapters.Capabilities {
	return adapters.Capabilities{SessionID: true, Duration: true}
}

func (a *Adapter) Discover(ctx context.Context) ([]adapters.Source, error) {
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

func supportsThreads(ctx context.Context, path string) (bool, error) {
	db, err := adapters.OpenReadOnly(ctx, path)
	if err != nil {
		return false, err
	}
	defer db.Close()
	return adapters.HasTable(ctx, db, "threads")
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
	if hasCWD, err := adapters.HasColumn(ctx, db, "threads", "cwd"); err != nil {
		return adapters.ParseResult{}, err
	} else if hasCWD {
		projectExpression = "cwd"
	}
	rows, err := db.QueryContext(ctx, `
SELECT id, created_at_ms, updated_at_ms, COALESCE(model_provider, ''), COALESCE(model, ''), tokens_used, `+projectExpression+`
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
		var (
			sessionID, provider, model, workingDirectory string
			createdAt, updatedAt, tokens                 int64
		)
		if err := rows.Scan(&sessionID, &createdAt, &updatedAt, &provider, &model, &tokens, &workingDirectory); err != nil {
			return adapters.ParseResult{}, err
		}
		provider = strings.TrimSpace(provider)
		if provider == "" {
			provider = "unknown"
		}
		model = a.NormalizeModel(model)
		if model == "" {
			model = "unknown"
		}
		timestamp := adapters.UnixTime(updatedAt)
		if updatedAt <= 0 {
			timestamp = adapters.UnixTime(createdAt)
		}
		events = append(events, usage.Event{
			SchemaVersion: usage.SchemaVersion,
			EventID:       usage.DeterministicID(machineID, adapterID, sourceIdentity, 0, "snapshot", sessionID),
			Timestamp:     timestamp,
			MachineID:     machineID,
			SessionID:     sessionID,
			Project:       usage.NormalizeProject(workingDirectory),
			Provider:      provider,
			Model:         model,
			Tool:          "codex",
			TotalTokens:   usage.Int64(tokens),
			DurationMS:    adapters.DurationMS(createdAt, updatedAt),
			Currency:      "USD",
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: adapterID, AdapterVersion: adapterVersion, Identity: sourceIdentity},
		})
	}
	return adapters.ParseResult{Events: events}, rows.Err()
}

var _ adapters.Adapter = (*Adapter)(nil)
