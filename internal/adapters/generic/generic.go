// Package generic reads normalized usage events from explicitly configured
// JSONL files. It is the extension path for tools without a native adapter.
package generic

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tokemon/tokemon/internal/adapters"
	"github.com/tokemon/tokemon/internal/usage"
)

const (
	adapterID      = "generic-jsonl"
	adapterVersion = "0.2.0"
)

// Adapter discovers only the explicit paths and filepath.Glob patterns passed
// to New. It never recursively scans a home directory.
type Adapter struct {
	patterns []string
}

func New(patterns ...string) *Adapter {
	return &Adapter{patterns: append([]string(nil), patterns...)}
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
		ReasoningTokens: true, Cost: true, SessionID: true, Duration: true,
	}
}

func (a *Adapter) Discover(ctx context.Context) ([]adapters.Source, error) {
	seen := make(map[string]struct{})
	var paths []string
	for _, pattern := range a.patterns {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		pattern, err := expandHome(pattern)
		if err != nil {
			return nil, err
		}
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid generic JSONL pattern %q: %w", pattern, err)
		}
		for _, path := range matches {
			absolute, err := filepath.Abs(path)
			if err != nil {
				return nil, err
			}
			if _, ok := seen[absolute]; ok {
				continue
			}
			info, err := os.Stat(absolute)
			if errors.Is(err, os.ErrNotExist) || (err == nil && !info.Mode().IsRegular()) {
				continue
			}
			if err != nil {
				return nil, err
			}
			seen[absolute] = struct{}{}
			paths = append(paths, absolute)
		}
	}
	sort.Strings(paths)
	sources := make([]adapters.Source, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		sources = append(sources, adapters.Source{Path: path, Identity: sourceIdentity(path, info)})
	}
	return sources, nil
}

func expandHome(pattern string) (string, error) {
	if pattern != "~" && !strings.HasPrefix(pattern, "~/") && !strings.HasPrefix(pattern, `~\`) {
		return pattern, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if pattern == "~" {
		return home, nil
	}
	return filepath.Join(home, pattern[2:]), nil
}

func (a *Adapter) Parse(ctx context.Context, source adapters.Source, request adapters.ParseRequest) (adapters.ParseResult, error) {
	machineID := strings.TrimSpace(request.MachineID)
	if machineID == "" {
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
		identity = sourceIdentity(source.Path, info)
	}
	offset := request.Cursor.Offset
	if request.Cursor.Identity != identity || offset < 0 || offset > info.Size() {
		offset = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return adapters.ParseResult{}, err
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
		position += int64(len(line))
		if len(strings.TrimSpace(string(line))) > 0 {
			var event usage.Event
			if err := json.Unmarshal(line, &event); err != nil {
				return adapters.ParseResult{}, fmt.Errorf("%s byte %d: %w", filepath.Base(source.Path), lineOffset, err)
			}
			event.MachineID = machineID
			event.Model = a.NormalizeModel(event.Model)
			event.Source = usage.Source{Adapter: adapterID, AdapterVersion: adapterVersion, Identity: identity, Offset: lineOffset}
			if strings.TrimSpace(event.EventID) == "" {
				event.EventID = usage.DeterministicID(machineID, adapterID, identity, lineOffset, event.Timestamp.UTC().Format(timeFormat), event.SessionID)
			}
			if err := event.Validate(); err != nil {
				return adapters.ParseResult{}, fmt.Errorf("%s byte %d: %w", filepath.Base(source.Path), lineOffset, err)
			}
			events = append(events, event)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return adapters.ParseResult{}, readErr
		}
	}
	return adapters.ParseResult{Events: events, Cursor: adapters.Cursor{Identity: identity, Offset: position}}, nil
}

const timeFormat = "2006-01-02T15:04:05.999999999Z07:00"

var _ adapters.Adapter = (*Adapter)(nil)
