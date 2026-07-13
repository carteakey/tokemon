// Package antigravity reads metadata-only generation counters from Antigravity
// conversation databases. It deliberately never reads the transcript tables or
// the prompt/response payloads stored elsewhere in Antigravity's data directory.
package antigravity

import (
	"context"
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
	adapterID      = "antigravity"
	adapterVersion = "0.1.0"
)

type Adapter struct{ roots []string }

func New(home string) *Adapter {
	return &Adapter{roots: []string{
		filepath.Join(home, ".gemini", "antigravity-cli", "conversations"),
		filepath.Join(home, ".gemini", "antigravity", "conversations"),
		filepath.Join(home, ".gemini", "antigravity-ide", "conversations"),
	}}
}

func (a *Adapter) ID() string { return adapterID }

func (a *Adapter) NormalizeModel(raw string) string {
	if model := strings.TrimSpace(raw); model != "" {
		return model
	}
	return "unknown"
}

func (a *Adapter) Capabilities() adapters.Capabilities {
	return adapters.Capabilities{InputTokens: true, OutputTokens: true, SessionID: true}
}

func (a *Adapter) Discover(ctx context.Context) ([]adapters.Source, error) {
	var sources []adapters.Source
	for _, root := range a.roots {
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
			if entry.IsDir() || !strings.EqualFold(filepath.Ext(path), ".db") {
				return nil
			}
			supported, err := supportsGenerationMetadata(ctx, path)
			if err == nil && supported {
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

func supportsGenerationMetadata(ctx context.Context, path string) (bool, error) {
	db, err := adapters.OpenReadOnly(ctx, path)
	if err != nil {
		return false, err
	}
	defer db.Close()
	return adapters.HasTable(ctx, db, "gen_metadata")
}

func (a *Adapter) Parse(ctx context.Context, source adapters.Source, request adapters.ParseRequest) (adapters.ParseResult, error) {
	if strings.TrimSpace(request.MachineID) == "" {
		return adapters.ParseResult{}, fmt.Errorf("machine ID is required")
	}
	db, err := adapters.OpenReadOnly(ctx, source.Path)
	if err != nil {
		return adapters.ParseResult{}, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT idx, data FROM gen_metadata WHERE data IS NOT NULL ORDER BY idx`)
	if err != nil {
		return adapters.ParseResult{}, fmt.Errorf("read Antigravity generation metadata: %w", err)
	}
	defer rows.Close()

	sessionID := strings.TrimSuffix(filepath.Base(source.Path), filepath.Ext(source.Path))
	identity := source.Identity
	if identity == "" {
		identity = adapters.HashIdentity(source.Path)
	}
	var events []usage.Event
	for rows.Next() {
		var idx int64
		var data []byte
		if err := rows.Scan(&idx, &data); err != nil {
			return adapters.ParseResult{}, err
		}
		metadata, ok := decodeGenerationMetadata(data)
		if !ok || metadata.InputTokens+metadata.OutputTokens <= 0 || metadata.Timestamp.IsZero() {
			continue
		}
		events = append(events, usage.Event{
			SchemaVersion: usage.SchemaVersion,
			EventID:       usage.DeterministicID(request.MachineID, adapterID, identity, idx, metadata.Timestamp.Format(time.RFC3339Nano), sessionID),
			Timestamp:     metadata.Timestamp,
			MachineID:     request.MachineID,
			SessionID:     sessionID,
			Provider:      providerForModel(metadata.Model),
			Model:         a.NormalizeModel(metadata.Model),
			Tool:          "antigravity",
			InputTokens:   usage.Int64(metadata.InputTokens),
			OutputTokens:  usage.Int64(metadata.OutputTokens),
			TotalTokens:   usage.Int64(metadata.InputTokens + metadata.OutputTokens),
			Currency:      "USD",
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: adapterID, AdapterVersion: adapterVersion, Identity: identity, Offset: idx},
		})
	}
	return adapters.ParseResult{Events: events}, rows.Err()
}

func providerForModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(model, "claude"):
		return "anthropic"
	case strings.HasPrefix(model, "gpt"), strings.HasPrefix(model, "o1"), strings.HasPrefix(model, "o3"), strings.HasPrefix(model, "o4"):
		return "openai"
	case strings.HasPrefix(model, "gemini"):
		return "google"
	default:
		return "unknown"
	}
}

// generationMetadata contains only fields Tokemon is allowed to retain. The
// Antigravity schema is internal protobuf, so decoding is intentionally narrow:
// the outer generation record's field 1 contains generation stats; its field 4
// contains output/input counters, field 9 contains a protobuf timestamp, and
// field 19 contains the model ID. Unknown fields are skipped.
type generationMetadata struct {
	InputTokens  int64
	OutputTokens int64
	Model        string
	Timestamp    time.Time
}

func decodeGenerationMetadata(data []byte) (generationMetadata, bool) {
	outer, ok := protobufBytesField(data, 1)
	if !ok {
		return generationMetadata{}, false
	}
	stats, ok := protobufBytesField(outer, 4)
	if !ok {
		return generationMetadata{}, false
	}
	output, _ := protobufVarintField(stats, 1)
	input, _ := protobufVarintField(stats, 2)
	model, _ := protobufBytesField(outer, 19)
	timestampMessage, _ := protobufBytesField(outer, 9)
	seconds, _ := protobufVarintField(timestampMessage, 4, 1)
	nanos, _ := protobufVarintField(timestampMessage, 4, 2)
	if seconds == 0 {
		return generationMetadata{}, false
	}
	return generationMetadata{
		InputTokens: int64(input), OutputTokens: int64(output), Model: string(model),
		Timestamp: time.Unix(int64(seconds), int64(nanos)).UTC(),
	}, true
}

func protobufBytesField(data []byte, path ...uint64) ([]byte, bool) {
	for i, field := range path {
		value, ok := protobufField(data, field, 2)
		if !ok {
			return nil, false
		}
		data = value
		if i == len(path)-1 {
			return data, true
		}
	}
	return nil, false
}

func protobufVarintField(data []byte, path ...uint64) (uint64, bool) {
	for i, field := range path {
		if i == len(path)-1 {
			value, ok := protobufField(data, field, 0)
			if !ok {
				return 0, false
			}
			result, _, ok := consumeVarint(value)
			return result, ok
		}
		var ok bool
		data, ok = protobufBytesField(data, field)
		if !ok {
			return 0, false
		}
	}
	return 0, false
}

func protobufField(data []byte, wanted, wantedWire uint64) ([]byte, bool) {
	for len(data) > 0 {
		key, n, ok := consumeVarint(data)
		if !ok {
			return nil, false
		}
		data = data[n:]
		field, wire := key>>3, key&7
		switch wire {
		case 0:
			_, size, ok := consumeVarint(data)
			if !ok {
				return nil, false
			}
			if field == wanted && wire == wantedWire {
				return data[:size], true
			}
			data = data[size:]
		case 1:
			if len(data) < 8 {
				return nil, false
			}
			data = data[8:]
		case 2:
			length, size, ok := consumeVarint(data)
			if !ok || length > uint64(len(data)-size) {
				return nil, false
			}
			value := data[size : size+int(length)]
			if field == wanted && wire == wantedWire {
				return value, true
			}
			data = data[size+int(length):]
		case 5:
			if len(data) < 4 {
				return nil, false
			}
			data = data[4:]
		default:
			return nil, false
		}
	}
	return nil, false
}

func consumeVarint(data []byte) (uint64, int, bool) {
	var value uint64
	for i, b := range data {
		if i == 10 || i == 9 && b > 1 {
			return 0, 0, false
		}
		value |= uint64(b&0x7f) << (7 * i)
		if b < 0x80 {
			return value, i + 1, true
		}
	}
	return 0, 0, false
}

var _ adapters.Adapter = (*Adapter)(nil)
