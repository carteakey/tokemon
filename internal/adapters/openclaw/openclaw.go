// Package openclaw reads metadata-only token usage from OpenClaw session
// transcripts. It never copies transcript content, tool calls, or arguments
// into normalized Tokemon events.
package openclaw

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tokemon/tokemon/internal/adapters"
	"github.com/tokemon/tokemon/internal/usage"
)

const (
	adapterID      = "openclaw"
	adapterVersion = "0.1.0"
)

// Adapter reads OpenClaw's per-agent session transcripts. OpenClaw keeps
// durable transcripts at ~/.openclaw/agents/<agent>/sessions/*.jsonl.
type Adapter struct {
	root string
}

func New(home string) *Adapter {
	return &Adapter{root: filepath.Join(home, ".openclaw", "agents")}
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
		ReasoningTokens: true, Cost: true, SessionID: true,
	}
}

func (a *Adapter) Discover(ctx context.Context) ([]adapters.Source, error) {
	paths, err := filepath.Glob(filepath.Join(a.root, "*", "sessions", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)

	sources := make([]adapters.Source, 0, len(paths))
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.HasSuffix(path, ".trajectory.jsonl") {
			// Codex mirrors created alongside OpenClaw sessions are not OpenClaw
			// transcripts and must not be counted a second time.
			continue
		}
		info, err := os.Stat(path)
		if os.IsNotExist(err) || (err == nil && !info.Mode().IsRegular()) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sources = append(sources, adapters.Source{
			Path: path, Identity: a.sourceIdentity(path),
		})
	}
	return sources, nil
}

func (a *Adapter) Parse(ctx context.Context, source adapters.Source, request adapters.ParseRequest) (adapters.ParseResult, error) {
	if strings.TrimSpace(request.MachineID) == "" {
		return adapters.ParseResult{}, errors.New("machine ID is required")
	}

	file, err := os.Open(source.Path)
	if err != nil {
		return adapters.ParseResult{}, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return adapters.ParseResult{}, err
	}
	identity := source.Identity
	if identity == "" {
		identity = a.sourceIdentity(source.Path)
	}
	header, headerBytes, err := readHeader(file)
	if err != nil {
		return adapters.ParseResult{}, err
	}
	cursorIdentity := makeCursorIdentity(identity, headerBytes)
	offset := request.Cursor.Offset
	lineNumber := request.Cursor.Line
	if request.Cursor.Identity != "" && request.Cursor.Identity != cursorIdentity {
		offset = 0
		lineNumber = 0
	}
	if offset < 0 || offset > info.Size() || lineNumber < 0 {
		offset = 0
		lineNumber = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return adapters.ParseResult{}, err
	}

	sessionID := firstNonEmpty(header.ID, header.SessionID, header.SessionIDAlt, sessionIDFromPath(source.Path))
	project := usage.NormalizeProject(header.CWD)
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
			if errors.Is(readErr, io.EOF) && !bytes.HasSuffix(line, []byte{'\n'}) {
				// OpenClaw can be writing the final record while the agent polls.
				// Leave it untouched so the next pass retries the complete line.
				position = lineOffset
				lineNumber--
				break
			}
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return adapters.ParseResult{}, readErr
			}
			// A malformed historical line should not hide later assistant
			// usage records from an otherwise readable OpenClaw transcript.
			continue
		}

		if record.Type != "message" || record.Message == nil || !strings.EqualFold(record.Message.Role, "assistant") {
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				return adapters.ParseResult{}, readErr
			}
			continue
		}
		rawUsage := record.Message.Usage
		if rawUsage == nil {
			rawUsage = record.Usage
		}
		if rawUsage == nil || !rawUsage.HasNonZeroTokens() {
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				return adapters.ParseResult{}, readErr
			}
			continue
		}

		timestamp, err := parseTimestamp(record.Timestamp)
		if err != nil {
			timestamp, err = parseTimestamp(record.Message.Timestamp)
		}
		if err != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				return adapters.ParseResult{}, readErr
			}
			continue
		}

		currentSessionID := firstNonEmpty(record.SessionID, record.SessionIDAlt, sessionID)
		currentProject := firstNonEmpty(
			usage.NormalizeProject(record.CWD),
			usage.NormalizeProject(record.Message.CWD),
			project,
		)
		provider := firstNonEmpty(record.Message.Provider, record.Provider, "unknown")
		model := a.NormalizeModel(firstNonEmpty(record.Message.Model, record.Model))
		event := normalize(
			rawUsage,
			request.MachineID,
			identity,
			lineNumber,
			record.ID,
			timestamp,
			currentSessionID,
			currentProject,
			provider,
			model,
		)
		events = append(events, event)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return adapters.ParseResult{}, readErr
		}
	}

	return adapters.ParseResult{
		Events: events,
		Cursor: adapters.Cursor{Identity: cursorIdentity, Offset: position, Line: lineNumber},
	}, nil
}

type transcriptHeader struct {
	Type         string `json:"type"`
	ID           string `json:"id"`
	SessionID    string `json:"sessionId"`
	SessionIDAlt string `json:"session_id"`
	CWD          string `json:"cwd"`
}

type transcriptRecord struct {
	Type         string          `json:"type"`
	ID           string          `json:"id"`
	Timestamp    json.RawMessage `json:"timestamp"`
	SessionID    string          `json:"sessionId"`
	SessionIDAlt string          `json:"session_id"`
	CWD          string          `json:"cwd"`
	Provider     string          `json:"provider"`
	Model        string          `json:"model"`
	Usage        *tokenUsage     `json:"usage"`
	Message      *messageRecord  `json:"message"`
}

type messageRecord struct {
	Role      string          `json:"role"`
	Timestamp json.RawMessage `json:"timestamp"`
	CWD       string          `json:"cwd"`
	Provider  string          `json:"provider"`
	Model     string          `json:"model"`
	Usage     *tokenUsage     `json:"usage"`
}

type tokenUsage struct {
	Input       *int64     `json:"input"`
	Output      *int64     `json:"output"`
	CacheRead   *int64     `json:"cacheRead"`
	CacheWrite  *int64     `json:"cacheWrite"`
	Reasoning   *int64     `json:"reasoningTokens"`
	TotalTokens *int64     `json:"totalTokens"`
	Total       *int64     `json:"total"`
	Cost        *usageCost `json:"cost"`
}

type usageCost struct {
	Total *float64 `json:"total"`
}

func (u *tokenUsage) HasNonZeroTokens() bool {
	if u == nil {
		return false
	}
	for _, value := range []*int64{u.Input, u.Output, u.CacheRead, u.CacheWrite, u.Reasoning, u.TotalTokens, u.Total} {
		if value != nil && *value > 0 {
			return true
		}
	}
	return u.Cost != nil && u.Cost.Total != nil && *u.Cost.Total > 0
}

func normalize(raw *tokenUsage, machineID, identity string, lineNumber int64, recordID string, timestamp time.Time, sessionID, project, provider, model string) usage.Event {
	input := safeToken(raw.Input)
	output := safeToken(raw.Output)
	cacheRead := safeToken(raw.CacheRead)
	cacheWrite := safeToken(raw.CacheWrite)
	reasoning := safeToken(raw.Reasoning)
	total := safeToken(firstPointer(raw.TotalTokens, raw.Total))
	accuracy := usage.AccuracyUnknown
	if total != nil {
		accuracy = usage.AccuracyReported
	} else if input != nil && output != nil && cacheRead != nil && cacheWrite != nil {
		value := *input + *output + *cacheRead + *cacheWrite
		if reasoning != nil {
			value += *reasoning
		}
		total = usage.Int64(value)
		accuracy = usage.AccuracyDerived
	}

	event := usage.Event{
		SchemaVersion: usage.SchemaVersion,
		EventID: usage.DeterministicID(
			machineID, adapterID, identity, lineNumber,
			timestamp.UTC().Format(time.RFC3339Nano),
			firstNonEmpty(recordID, sessionID),
		),
		Timestamp:        timestamp,
		MachineID:        machineID,
		SessionID:        sessionID,
		Project:          project,
		Provider:         provider,
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
		Source:           usage.Source{Adapter: adapterID, AdapterVersion: adapterVersion, Identity: identity, Offset: lineNumber},
	}
	if raw.Cost != nil && raw.Cost.Total != nil && *raw.Cost.Total > 0 {
		cost := *raw.Cost.Total
		event.Cost = usage.Float64(cost)
		event.CostEstimated = true
	}
	return event
}

func safeToken(value *int64) *int64 {
	if value == nil || *value < 0 {
		return nil
	}
	return usage.Int64(*value)
}

func firstPointer(values ...*int64) *int64 {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func readHeader(file *os.File) (transcriptHeader, []byte, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return transcriptHeader{}, nil, err
	}
	line, err := bufio.NewReaderSize(file, 64*1024).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return transcriptHeader{}, nil, err
	}
	var header transcriptHeader
	if json.Unmarshal(line, &header) != nil || header.Type != "session" {
		header = transcriptHeader{}
	}
	return header, line, nil
}

func (a *Adapter) sourceIdentity(path string) string {
	relative, err := filepath.Rel(a.root, path)
	if err != nil {
		relative = filepath.Base(path)
	}
	return adapters.HashIdentity(adapterID + ":" + filepath.ToSlash(relative))
}

func makeCursorIdentity(sourceIdentity string, header []byte) string {
	digest := sha256.Sum256(header)
	return adapters.HashIdentity(adapterID + ":" + sourceIdentity + ":" + hex.EncodeToString(digest[:]))
}

func sessionIDFromPath(path string) string {
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

func parseTimestamp(raw json.RawMessage) (time.Time, error) {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return time.Time{}, errors.New("timestamp is missing")
	}
	if strings.HasPrefix(value, "\"") {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return time.Time{}, err
		}
		return time.Parse(time.RFC3339Nano, text)
	}
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	if number >= 1_000_000_000_000 || number <= -1_000_000_000_000 {
		return time.UnixMilli(number).UTC(), nil
	}
	return time.Unix(number, 0).UTC(), nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

var _ adapters.Adapter = (*Adapter)(nil)
