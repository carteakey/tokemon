// Package adapters contains the small, metadata-only integrations that turn
// local tool state into Tokemon usage events.
package adapters

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/tokemon/tokemon/internal/usage"
)

const MaxRecordBytes = 1 << 20

// ReadRecord reads one newline-delimited provider record without allowing a
// malformed line to grow without bound in memory.
func ReadRecord(reader *bufio.Reader) ([]byte, error) {
	var record []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		record = append(record, chunk...)
		if len(record) > MaxRecordBytes {
			return nil, fmt.Errorf("provider record exceeds %d bytes", MaxRecordBytes)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == nil || err == io.EOF {
			return record, err
		}
		return record, err
	}
}

// HashIdentity turns a local source identity into a safe value for an event.
// In particular, encoded project paths must never be sent as source metadata.
func HashIdentity(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}

// Source is a local usage database or file discovered by an adapter. Paths are
// used only on the machine running the agent and are never put in events.
type Source struct {
	Path     string
	Identity string
}

// Cursor is the last position successfully uploaded for one source identity.
// An identity change or an offset beyond the current file size resets parsing
// to the beginning, which handles replacement and truncation safely.
type Cursor struct {
	Identity string `json:"identity"`
	Offset   int64  `json:"offset"`
	Line     int64  `json:"line,omitempty"`
}

type ParseRequest struct {
	MachineID string
	Cursor    Cursor
}

type ParseResult struct {
	Events []usage.Event
	Cursor Cursor
}

// Capabilities describes which fields an adapter can report safely.
type Capabilities struct {
	InputTokens     bool `json:"input_tokens"`
	OutputTokens    bool `json:"output_tokens"`
	CacheTokens     bool `json:"cache_tokens"`
	ReasoningTokens bool `json:"reasoning_tokens"`
	Cost            bool `json:"cost"`
	SessionID       bool `json:"session_id"`
	Duration        bool `json:"duration"`
}

// Adapter discovers one provider's local usage records and normalizes them.
// Parse may return aggregate session snapshots; the server keeps their
// deterministic event IDs stable so later scans refresh those snapshots.
type Adapter interface {
	ID() string
	Discover(context.Context) ([]Source, error)
	Parse(context.Context, Source, ParseRequest) (ParseResult, error)
	NormalizeModel(string) string
	Capabilities() Capabilities
}

// SourceReport describes one discovery or parse attempt made by the agent.
type SourceReport struct {
	Adapter string
	Path    string
	Source  Source
	Events  int
	Cursor  Cursor
	Err     error
}

// Collect discovers every source exposed by the supplied adapters and parses
// all sources that are available. One broken source does not hide data from
// another adapter.
func Collect(ctx context.Context, list []Adapter, machineID string) ([]usage.Event, []SourceReport) {
	return CollectWithCursors(ctx, list, machineID, nil)
}

// CollectWithCursors discovers and parses sources using the last committed
// cursor for each adapter/path pair. A source parse failure is isolated so
// healthy sources can still upload their metadata.
func CollectWithCursors(ctx context.Context, list []Adapter, machineID string, cursors map[string]Cursor) ([]usage.Event, []SourceReport) {
	var events []usage.Event
	var reports []SourceReport
	for _, adapter := range list {
		sources, err := adapter.Discover(ctx)
		if err != nil {
			reports = append(reports, SourceReport{Adapter: adapter.ID(), Err: err})
			continue
		}
		for _, source := range sources {
			report := SourceReport{Adapter: adapter.ID(), Path: source.Path, Source: source}
			cursor := Cursor{}
			if cursors != nil {
				cursor = cursors[adapter.ID()+"\x00"+source.Path]
			}
			parsed, err := adapter.Parse(ctx, source, ParseRequest{MachineID: machineID, Cursor: cursor})
			if err != nil {
				report.Err = err
				reports = append(reports, report)
				continue
			}
			report.Events = len(parsed.Events)
			report.Cursor = parsed.Cursor
			events = append(events, parsed.Events...)
			reports = append(reports, report)
		}
	}
	return events, reports
}
