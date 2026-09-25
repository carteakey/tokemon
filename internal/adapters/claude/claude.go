// Package claude reads Claude Code session transcript usage metadata.
package claude

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
	root        string
	directories *adapters.DirectoryCache
}

func New(home string) *Adapter {
	return &Adapter{root: filepath.Join(home, ".claude", "projects"), directories: adapters.NewDirectoryCache()}
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
	paths, err := a.directories.Paths(ctx, a.root, ".jsonl")
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, nil
	}
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
	info, err := file.Stat()
	if err != nil {
		return adapters.ParseResult{}, err
	}
	cursorID := cursorIdentity(source.Path, info)
	offset := request.Cursor.Offset
	lineNumber := request.Cursor.Line
	if (request.Cursor.Identity != "" && request.Cursor.Identity != cursorID) || offset < 0 || offset > info.Size() {
		offset = 0
		lineNumber = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return adapters.ParseResult{}, err
	}
	if offset > 0 && lineNumber > 0 {
		replay, err := nextLineIsDuration(file, offset)
		if err != nil {
			return adapters.ParseResult{}, err
		}
		if replay {
			if start, err := previousLineStart(file, offset); err != nil {
				return adapters.ParseResult{}, err
			} else if start < offset {
				offset = start
				lineNumber--
				if _, err := file.Seek(offset, io.SeekStart); err != nil {
					return adapters.ParseResult{}, err
				}
			}
		}
	}
	reader := bufio.NewReaderSize(file, 64*1024)
	position := offset
	var events []usage.Event
	for {
		if err := ctx.Err(); err != nil {
			return adapters.ParseResult{}, err
		}
		lineOffset := position
		line, readErr := reader.ReadBytes('\n')
		if len(line) == 0 && errors.Is(readErr, io.EOF) {
			break
		}
		position += int64(len(line))
		lineNumber++
		var record transcriptRecord
		if err := json.Unmarshal(line, &record); err != nil {
			// Claude transcripts can contain partially written final records while
			// a session is active. The next poll will see the complete record.
			if errors.Is(readErr, io.EOF) && !bytes.HasSuffix(line, []byte{'\n'}) {
				position = lineOffset
				lineNumber--
				break
			}
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return adapters.ParseResult{}, readErr
			}
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
			sessionID = usage.HashSessionID(source.Path)
		} else {
			sessionID = usage.NormalizeSessionID(sessionID)
		}
		event := normalize(record, rawUsage, machineID, identity, lineNumber, sessionID, timestamp)
		events = append(events, event)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return adapters.ParseResult{}, readErr
		}
	}
	return adapters.ParseResult{Events: events, Cursor: adapters.Cursor{Identity: cursorID, Offset: position, Line: lineNumber}}, nil
}

func nextLineIsDuration(file *os.File, offset int64) (bool, error) {
	const chunkSize = 64 * 1024
	buffer := make([]byte, chunkSize)
	n, err := file.ReadAt(buffer, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	line := buffer[:n]
	if end := bytes.IndexByte(line, '\n'); end >= 0 {
		line = line[:end]
	}
	var record transcriptRecord
	if err := json.Unmarshal(line, &record); err != nil {
		return false, nil
	}
	return record.Type == "system" && record.Subtype == "turn_duration" && record.DurationMS != nil, nil
}

func previousLineStart(file *os.File, offset int64) (int64, error) {
	if offset <= 0 {
		return 0, nil
	}
	lastByte := []byte{0}
	if _, err := file.ReadAt(lastByte, offset-1); err != nil {
		return 0, err
	}
	if lastByte[0] != '\n' {
		return offset, nil
	}

	const chunkSize = 64 * 1024
	buffer := make([]byte, chunkSize)
	position := offset
	newlines := 0
	for position > 0 {
		readSize := int64(len(buffer))
		if readSize > position {
			readSize = position
		}
		position -= readSize
		n, err := file.ReadAt(buffer[:readSize], position)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		for index := n - 1; index >= 0; index-- {
			if buffer[index] != '\n' {
				continue
			}
			newlines++
			if newlines == 2 {
				return position + int64(index) + 1, nil
			}
		}
		if n == 0 {
			break
		}
	}
	return 0, nil
}

func (a *Adapter) sourceIdentity(path string) string {
	relative, err := filepath.Rel(a.root, path)
	if err != nil {
		relative = filepath.Base(path)
	}
	return adapters.HashIdentity(adapterID + ":" + filepath.ToSlash(relative))
}

func cursorIdentity(path string, info os.FileInfo) string {
	return fileIdentity(adapterID, path, info)
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
