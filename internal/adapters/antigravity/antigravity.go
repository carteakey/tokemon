// Package antigravity reads metadata-only generation counters and project labels from Antigravity
// conversation databases. It deliberately never reads the transcript tables or
// the prompt/response payloads stored elsewhere in Antigravity's data directory.
package antigravity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tokemon/tokemon/internal/adapters"
	"github.com/tokemon/tokemon/internal/usage"
)

const (
	adapterID      = "antigravity"
	adapterVersion = "0.3.0"
)

type fileSignature struct {
	exists  bool
	size    int64
	modTime int64
}

type sourceSignature struct {
	database fileSignature
	wal      fileSignature
}

type directoryEntry struct {
	signature fileSignature
	paths     []string
}

type parseEntry struct {
	signature     sourceSignature
	identity      string
	requestCursor adapters.Cursor
	resultCursor  adapters.Cursor
	err           error
}

type Adapter struct {
	roots []string
	mu    sync.Mutex

	directories map[string]directoryEntry
	parsed      map[string]parseEntry
	open        func(context.Context, string) (*sql.DB, error)
	readDir     func(string) ([]os.DirEntry, error)
}

func New(home string) *Adapter {
	return &Adapter{
		roots: []string{
			filepath.Join(home, ".gemini", "antigravity-cli", "conversations"),
			filepath.Join(home, ".gemini", "antigravity", "conversations"),
			filepath.Join(home, ".gemini", "antigravity-ide", "conversations"),
		},
		directories: make(map[string]directoryEntry),
		parsed:      make(map[string]parseEntry),
		open:        adapters.OpenReadOnly,
		readDir:     os.ReadDir,
	}
}

func (a *Adapter) ID() string { return adapterID }

func (a *Adapter) NormalizeModel(raw string) string {
	if model := strings.TrimSpace(raw); model != "" {
		return model
	}
	return "unknown"
}

func (a *Adapter) Capabilities() adapters.Capabilities {
	return adapters.Capabilities{InputTokens: true, OutputTokens: true, CacheTokens: true, ReasoningTokens: true, SessionID: true}
}

func (a *Adapter) Discover(ctx context.Context) ([]adapters.Source, error) {
	var sources []adapters.Source
	seen := make(map[string]struct{})
	for _, root := range a.roots {
		paths, err := a.candidatePaths(root)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, path := range paths {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			seen[path] = struct{}{}
			// We skip the expensive database probe here.
			// Just assume every candidate .db is a source, and we will verify table existence in Parse.
			sources = append(sources, adapters.Source{Path: path, Identity: cursorIdentity(path)})
		}
	}
	a.mu.Lock()
	for path := range a.parsed {
		if _, ok := seen[path]; !ok {
			delete(a.parsed, path)
		}
	}
	a.mu.Unlock()
	sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })
	return sources, nil
}

func (a *Adapter) candidatePaths(root string) ([]string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("antigravity source root is not a directory: %s", root)
	}
	signature := signatureFromInfo(info)
	a.mu.Lock()
	cached, cachedOK := a.directories[root]
	a.mu.Unlock()
	if cachedOK && cached.signature == signature {
		return append([]string(nil), cached.paths...), nil
	}
	readDir := a.readDir
	if readDir == nil {
		readDir = os.ReadDir
	}
	entries, err := readDir(root)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(entry.Name()), ".db") {
			paths = append(paths, filepath.Join(root, entry.Name()))
		}
	}
	sort.Strings(paths)
	a.mu.Lock()
	a.directories[root] = directoryEntry{signature: signature, paths: append([]string(nil), paths...)}
	a.mu.Unlock()
	return paths, nil
}

func signatureFromInfo(info os.FileInfo) fileSignature {
	return fileSignature{exists: true, size: info.Size(), modTime: info.ModTime().UnixNano()}
}

func sourceSignatureForPath(path string) (sourceSignature, error) {
	info, err := os.Stat(path)
	if err != nil {
		return sourceSignature{}, err
	}
	signature := sourceSignature{database: signatureFromInfo(info)}
	if sidecar, err := os.Stat(path + "-wal"); err == nil {
		signature.wal = signatureFromInfo(sidecar)
	} else if !os.IsNotExist(err) {
		return sourceSignature{}, err
	}
	return signature, nil
}

func (a *Adapter) Parse(ctx context.Context, source adapters.Source, request adapters.ParseRequest) (adapters.ParseResult, error) {
	if strings.TrimSpace(request.MachineID) == "" {
		return adapters.ParseResult{}, fmt.Errorf("machine ID is required")
	}
	signature, err := sourceSignatureForPath(source.Path)
	if err != nil {
		return adapters.ParseResult{}, err
	}
	identity := cursorIdentity(source.Path)
	eventIdentity := adapters.HashIdentity(source.Path)
	start := request.Cursor.Offset
	if (request.Cursor.Identity != "" && request.Cursor.Identity != identity) || start < 0 {
		start = 0
	}
	a.mu.Lock()
	cached, cachedOK := a.parsed[source.Path]
	a.mu.Unlock()
	if cachedOK && cached.signature == signature && cached.identity == identity {
		if cached.err != nil && cached.requestCursor == request.Cursor {
			return adapters.ParseResult{}, cached.err
		}
		if cached.err == nil && cached.resultCursor == request.Cursor {
			return adapters.ParseResult{Cursor: cached.resultCursor}, nil
		}
	}
	open := a.open
	if open == nil {
		open = adapters.OpenReadOnly
	}
	db, err := open(ctx, source.Path)
	if err != nil {
		return a.rememberParse(source.Path, signature, identity, request.Cursor, adapters.ParseResult{}, err)
	}
	defer db.Close()

	// Check if this database actually supports generation metadata (contains the table 'gen_metadata')
	hasTable, err := adapters.HasTable(ctx, db, "gen_metadata")
	if err != nil {
		return a.rememberParse(source.Path, signature, identity, request.Cursor, adapters.ParseResult{}, err)
	}
	if !hasTable {
		// A regular SQLite database without the gen_metadata table is not an
		// Antigravity source. Cache the empty cursor so it is not reopened.
		result := adapters.ParseResult{Cursor: adapters.Cursor{Identity: identity, Offset: start}}
		a.mu.Lock()
		a.parsed[source.Path] = parseEntry{signature: signature, identity: identity, resultCursor: result.Cursor}
		a.mu.Unlock()
		return result, nil
	}

	project, err := projectFromDatabase(ctx, db)
	if err != nil {
		return a.rememberParse(source.Path, signature, identity, request.Cursor, adapters.ParseResult{}, err)
	}
	sessionID := usage.HashSessionID(source.Path)
	// Antigravity moved generation counters out of gen_metadata's record value
	// into the steps table. When the newest generation record no longer carries
	// its own timestamp, read the per-generation steps instead. Conversations
	// that still decode with the original layout keep the original path so
	// already-ingested history is not re-read or double-counted.
	stepsStart, useSteps, err := generationStepsStart(ctx, db, start)
	if err != nil {
		return a.rememberParse(source.Path, signature, identity, request.Cursor, adapters.ParseResult{}, err)
	}
	if useSteps {
		events, nextOffset, err := a.parseGenerationSteps(ctx, db, request, eventIdentity, sessionID, project, stepsStart)
		if err != nil {
			return a.rememberParse(source.Path, signature, identity, request.Cursor, adapters.ParseResult{}, err)
		}
		result := adapters.ParseResult{Events: events, Cursor: adapters.Cursor{Identity: identity, Offset: nextOffset}}
		if err := db.Close(); err != nil {
			return a.rememberParse(source.Path, signature, identity, request.Cursor, adapters.ParseResult{}, err)
		}
		if current, err := sourceSignatureForPath(source.Path); err == nil {
			signature = current
		}
		a.mu.Lock()
		a.parsed[source.Path] = parseEntry{signature: signature, identity: identity, resultCursor: result.Cursor}
		a.mu.Unlock()
		return result, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT idx, data FROM gen_metadata WHERE data IS NOT NULL AND idx >= ? ORDER BY idx`, start)
	if err != nil {
		err = fmt.Errorf("read Antigravity generation metadata: %w", err)
		return a.rememberParse(source.Path, signature, identity, request.Cursor, adapters.ParseResult{}, err)
	}
	defer rows.Close()
	var events []usage.Event
	nextOffset := start
	for rows.Next() {
		var idx int64
		var data []byte
		if err := rows.Scan(&idx, &data); err != nil {
			return a.rememberParse(source.Path, signature, identity, request.Cursor, adapters.ParseResult{}, err)
		}
		if idx >= nextOffset {
			nextOffset = idx + 1
		}
		metadata, ok := decodeGenerationMetadata(data)
		total, totalOK := metadataTotalTokens(metadata)
		if !ok || !totalOK || total <= 0 || metadata.Timestamp.IsZero() {
			continue
		}
		events = append(events, usage.Event{
			SchemaVersion:   usage.SchemaVersion,
			EventID:         usage.DeterministicID(request.MachineID, adapterID, eventIdentity, idx, metadata.Timestamp.Format(time.RFC3339Nano), sessionID),
			Timestamp:       metadata.Timestamp,
			MachineID:       request.MachineID,
			SessionID:       sessionID,
			Project:         project,
			Provider:        providerForModel(metadata.Model),
			Model:           a.NormalizeModel(metadata.Model),
			Tool:            "antigravity",
			InputTokens:     usage.Int64(metadata.InputTokens),
			OutputTokens:    usage.Int64(metadata.OutputTokens),
			CacheReadTokens: optionalInt64(metadata.CacheReadTokens, metadata.HasCacheRead),
			ReasoningTokens: optionalInt64(metadata.ReasoningTokens, metadata.HasReasoning),
			TotalTokens:     usage.Int64(total),
			Currency:        "USD",
			TokenAccuracy:   usage.AccuracyReported,
			Source:          usage.Source{Adapter: adapterID, AdapterVersion: adapterVersion, Identity: eventIdentity, Offset: idx},
		})
	}
	result := adapters.ParseResult{Events: events, Cursor: adapters.Cursor{Identity: identity, Offset: nextOffset}}
	if err := rows.Err(); err != nil {
		return a.rememberParse(source.Path, signature, identity, request.Cursor, adapters.ParseResult{}, err)
	}
	if err := rows.Close(); err != nil {
		return a.rememberParse(source.Path, signature, identity, request.Cursor, adapters.ParseResult{}, err)
	}
	if err := db.Close(); err != nil {
		return a.rememberParse(source.Path, signature, identity, request.Cursor, adapters.ParseResult{}, err)
	}
	if current, err := sourceSignatureForPath(source.Path); err == nil {
		signature = current
	}
	a.mu.Lock()
	a.parsed[source.Path] = parseEntry{signature: signature, identity: identity, resultCursor: result.Cursor}
	a.mu.Unlock()
	return result, nil
}

func cursorIdentity(path string) string {
	return adapters.HashIdentity(adapterID + "\x00" + adapterVersion + "\x00" + path)
}

// generationStepsStart decides whether a database needs the steps-based
// parser. Antigravity's older gen_metadata values carry their own timestamp
// and counters; newer values are a compact generation record without one, and
// the per-generation counters now live in steps.metadata. Only the databases
// whose newest record no longer decodes switch layouts.
//
// cursorOffset is the number of generation records already processed by the
// original parser. A conversation that began on the old layout reuses it as
// the number of steps already covered so upgrading does not re-emit history;
// a conversation that was always on the new layout has no ingested history
// and starts from zero.
func generationStepsStart(ctx context.Context, db *sql.DB, cursorOffset int64) (int64, bool, error) {
	hasSteps, err := adapters.HasTable(ctx, db, "steps")
	if err != nil {
		return 0, false, err
	}
	if !hasSteps {
		return 0, false, nil
	}
	hasStepType, err := adapters.HasColumn(ctx, db, "steps", "step_type")
	if err != nil {
		return 0, false, err
	}
	hasMetadata, err := adapters.HasColumn(ctx, db, "steps", "metadata")
	if err != nil {
		return 0, false, err
	}
	if !hasStepType || !hasMetadata {
		return 0, false, nil
	}
	var last []byte
	err = db.QueryRowContext(ctx, `SELECT data FROM gen_metadata WHERE data IS NOT NULL ORDER BY idx DESC LIMIT 1`).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if _, ok := decodeGenerationMetadata(last); ok {
		return 0, false, nil
	}
	start := cursorOffset
	if start < 0 {
		start = 0
	}
	var steps int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM steps WHERE step_type = 15 AND metadata IS NOT NULL`).Scan(&steps); err != nil {
		return 0, false, err
	}
	if start > steps {
		start = steps
	}
	return start, true, nil
}

// decodeStepMetadata reads the timestamp and usage counters Antigravity stores
// on an assistant step. Only these scalar fields are touched; step_payload and
// the transcript tables are never queried.
//
// Antigravity's usage protobuf (field 9 of steps.metadata) encodes:
//   - field 1: model output ceiling (constant per conversation; ignored)
//   - field 2: uncached input tokens
//   - field 3: total output tokens (field 9 thinking + field 10 visible text)
//   - field 5: cache-read input tokens (omitted on cold turns -> 0)
//   - field 9: reasoning/thinking output tokens (subset of field 3)
func decodeStepMetadata(data []byte) (generationMetadata, bool) {
	timestampMessage, ok := protobufBytesField(data, 1)
	if !ok {
		return generationMetadata{}, false
	}
	seconds, ok := protobufVarintField(timestampMessage, 1)
	if !ok || seconds == 0 {
		return generationMetadata{}, false
	}
	nanos, _ := protobufVarintField(timestampMessage, 2)
	usageMessage, ok := protobufBytesField(data, 9)
	if !ok {
		return generationMetadata{}, false
	}
	inputRaw, ok := protobufVarintField(usageMessage, 2)
	if !ok || inputRaw > math.MaxInt64 {
		return generationMetadata{}, false
	}
	// Field 1 is the model's configured output ceiling and is constant for a
	// conversation; field 3 is the generated output for this step.
	outputRaw, ok := protobufVarintField(usageMessage, 3)
	if !ok || outputRaw > math.MaxInt64 {
		return generationMetadata{}, false
	}
	cacheReadRaw, _ := protobufVarintField(usageMessage, 5)
	if cacheReadRaw > math.MaxInt64 {
		return generationMetadata{}, false
	}
	reasoningRaw, hasReasoning := protobufVarintField(usageMessage, 9)
	if hasReasoning && reasoningRaw > outputRaw {
		reasoningRaw = outputRaw
	}
	return generationMetadata{
		InputTokens:     int64(inputRaw),
		OutputTokens:    int64(outputRaw),
		CacheReadTokens: int64(cacheReadRaw),
		HasCacheRead:    true,
		ReasoningTokens: int64(reasoningRaw),
		HasReasoning:    hasReasoning,
		Timestamp:       time.Unix(int64(seconds), int64(nanos)).UTC(),
	}, true
}

// conversationModel returns the model recorded on the generation records. A
// conversation uses one model in practice, so the newest record wins.
func conversationModel(ctx context.Context, db *sql.DB) string {
	rows, err := db.QueryContext(ctx, `SELECT data FROM gen_metadata WHERE data IS NOT NULL ORDER BY idx DESC LIMIT 32`)
	if err != nil {
		return ""
	}
	defer rows.Close()
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return ""
		}
		for _, path := range [][]uint64{{3, 28}, {19}, {1, 19}, {1, 3, 28}} {
			if model, ok := protobufBytesField(data, path...); ok && len(model) > 0 {
				return string(model)
			}
		}
	}
	return ""
}

func (a *Adapter) parseGenerationSteps(ctx context.Context, db *sql.DB, request adapters.ParseRequest, eventIdentity, sessionID, project string, start int64) ([]usage.Event, int64, error) {
	model := conversationModel(ctx, db)
	rows, err := db.QueryContext(ctx, `SELECT metadata FROM steps WHERE step_type = 15 AND metadata IS NOT NULL ORDER BY idx`)
	if err != nil {
		return nil, start, fmt.Errorf("read Antigravity generation steps: %w", err)
	}
	defer rows.Close()
	var events []usage.Event
	rank := int64(0)
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, rank, err
		}
		current := rank
		rank++
		if current < start {
			continue
		}
		metadata, ok := decodeStepMetadata(data)
		total, totalOK := metadataTotalTokens(metadata)
		if !ok || !totalOK || total <= 0 {
			continue
		}
		events = append(events, usage.Event{
			SchemaVersion:   usage.SchemaVersion,
			EventID:         usage.DeterministicID(request.MachineID, adapterID, eventIdentity, current, metadata.Timestamp.Format(time.RFC3339Nano), sessionID),
			Timestamp:       metadata.Timestamp,
			MachineID:       request.MachineID,
			SessionID:       sessionID,
			Project:         project,
			Provider:        providerForModel(model),
			Model:           a.NormalizeModel(model),
			Tool:            "antigravity",
			InputTokens:     usage.Int64(metadata.InputTokens),
			OutputTokens:    usage.Int64(metadata.OutputTokens),
			CacheReadTokens: optionalInt64(metadata.CacheReadTokens, metadata.HasCacheRead),
			ReasoningTokens: optionalInt64(metadata.ReasoningTokens, metadata.HasReasoning),
			TotalTokens:     usage.Int64(total),
			Currency:        "USD",
			TokenAccuracy:   usage.AccuracyReported,
			Source:          usage.Source{Adapter: adapterID, AdapterVersion: adapterVersion, Identity: eventIdentity, Offset: current},
		})
	}
	return events, rank, rows.Err()
}

func projectFromDatabase(ctx context.Context, db *sql.DB) (string, error) {
	hasTable, err := adapters.HasTable(ctx, db, "trajectory_metadata_blob")
	if err != nil {
		return "", err
	}
	if !hasTable {
		return "", nil
	}
	var data []byte
	err = db.QueryRowContext(ctx, `SELECT data FROM trajectory_metadata_blob WHERE id = 'main'`).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read Antigravity project metadata: %w", err)
	}
	return decodeTrajectoryProject(data), nil
}

func decodeTrajectoryProject(data []byte) string {
	uriBytes, ok := protobufBytesField(data, 1, 1)
	if !ok {
		return ""
	}
	rawURI := strings.TrimSpace(string(uriBytes))
	if rawURI == "" {
		return ""
	}

	parsed, err := url.Parse(rawURI)
	if err != nil {
		return ""
	}
	projectPath := rawURI
	if strings.EqualFold(parsed.Scheme, "file") {
		projectPath = parsed.Path
	} else if parsed.Scheme != "" {
		return ""
	}
	return usage.NormalizeProject(projectPath)
}

func (a *Adapter) rememberParse(path string, signature sourceSignature, identity string, cursor adapters.Cursor, result adapters.ParseResult, err error) (adapters.ParseResult, error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return result, err
	}
	a.mu.Lock()
	a.parsed[path] = parseEntry{signature: signature, identity: identity, requestCursor: cursor, resultCursor: result.Cursor, err: err}
	a.mu.Unlock()
	return result, err
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
// contains uncached input (2), output (3, with legacy fallback 1), cache-read
// input (5), and reasoning output (9); field 9 contains a protobuf timestamp;
// and field 19 contains the model ID. Unknown fields are skipped.
type generationMetadata struct {
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
	HasCacheRead    bool
	ReasoningTokens int64
	HasReasoning    bool
	Model           string
	Timestamp       time.Time
}

func optionalInt64(value int64, present bool) *int64 {
	if !present {
		return nil
	}
	return usage.Int64(value)
}

func metadataTotalTokens(metadata generationMetadata) (int64, bool) {
	if metadata.InputTokens < 0 || metadata.OutputTokens < 0 || metadata.CacheReadTokens < 0 {
		return 0, false
	}
	if metadata.InputTokens > math.MaxInt64-metadata.CacheReadTokens {
		return 0, false
	}
	sum := metadata.InputTokens + metadata.CacheReadTokens
	if sum > math.MaxInt64-metadata.OutputTokens {
		return 0, false
	}
	return sum + metadata.OutputTokens, true
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
	// Field 3 is the step's actual output count (matching steps.metadata).
	// Field 1 is the conversation's output ceiling on real Antigravity records
	// and is kept only as a fallback when field 3 is absent.
	outputRaw, hasRealOutput := protobufVarintField(stats, 3)
	if !hasRealOutput {
		outputRaw, _ = protobufVarintField(stats, 1)
	}
	inputRaw, _ := protobufVarintField(stats, 2)
	cacheReadRaw, hasCacheField := protobufVarintField(stats, 5)
	if inputRaw > math.MaxInt64 || outputRaw > math.MaxInt64 || cacheReadRaw > math.MaxInt64 {
		return generationMetadata{}, false
	}
	reasoningRaw, hasReasoning := protobufVarintField(stats, 9)
	if hasReasoning && reasoningRaw > outputRaw {
		reasoningRaw = outputRaw
	}
	model, _ := protobufBytesField(outer, 19)
	timestampMessage, _ := protobufBytesField(outer, 9)
	seconds, _ := protobufVarintField(timestampMessage, 4, 1)
	nanos, _ := protobufVarintField(timestampMessage, 4, 2)
	if seconds == 0 {
		return generationMetadata{}, false
	}
	return generationMetadata{
		InputTokens:     int64(inputRaw),
		OutputTokens:    int64(outputRaw),
		CacheReadTokens: int64(cacheReadRaw),
		HasCacheRead:    hasRealOutput || hasCacheField,
		ReasoningTokens: int64(reasoningRaw),
		HasReasoning:    hasReasoning,
		Model:           string(model),
		Timestamp:       time.Unix(int64(seconds), int64(nanos)).UTC(),
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
