package usage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
)

// ErrSensitiveContent is returned before a payload is handed to an HTTP
// client. Callers can use errors.Is to distinguish a privacy rejection from a
// transport failure without logging the offending value.
var ErrSensitiveContent = errors.New("outbound payload contains disallowed sensitive content")

// Metadata keys are intentionally namespaced. A provider may retain a small
// amount of usage metadata, but an extension must opt into the public
// `tokemon_` namespace instead of forwarding arbitrary provider fields.
const metadataNamespace = "tokemon_"

var allowedEventFields = map[string]struct{}{
	"schema_version": {}, "event_id": {}, "timestamp": {}, "machine_id": {},
	"session_id": {}, "project": {}, "provider": {}, "model": {},
	"canonical_model": {}, "tool": {}, "input_tokens": {}, "output_tokens": {},
	"cache_read_tokens": {}, "cache_write_tokens": {}, "reasoning_tokens": {},
	"total_tokens": {}, "duration_ms": {}, "cost": {}, "cost_estimated": {},
	"currency": {}, "token_accuracy": {}, "source": {}, "metadata": {},
}

var allowedSourceFields = map[string]struct{}{
	"adapter": {}, "adapter_version": {}, "identity": {}, "offset": {},
}

// DecodeOutboundEvent strictly decodes the normalized JSONL format. Unknown
// top-level fields are rejected instead of being silently copied through a
// future struct field or an arbitrary provider payload.
func DecodeOutboundEvent(data []byte) (Event, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var fields map[string]json.RawMessage
	if err := decoder.Decode(&fields); err != nil {
		return Event{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return Event{}, errors.New("event JSON must contain one object")
	} else if !errors.Is(err, io.EOF) {
		// A second JSON value or malformed trailing bytes are not a normalized
		// JSONL record. Keep the diagnostic free of record contents.
		return Event{}, errors.New("event JSON contains trailing data")
	}
	for key := range fields {
		if _, ok := allowedEventFields[strings.ToLower(strings.TrimSpace(key))]; !ok {
			return Event{}, fmt.Errorf("%w: field %q is not approved", ErrSensitiveContent, key)
		}
	}
	var rawSource json.RawMessage
	for key, value := range fields {
		if strings.EqualFold(strings.TrimSpace(key), "source") {
			rawSource = value
			break
		}
	}
	if rawSource != nil {
		var sourceFields map[string]json.RawMessage
		if err := json.Unmarshal(rawSource, &sourceFields); err != nil {
			return Event{}, fmt.Errorf("%w: source must be an object", ErrSensitiveContent)
		}
		for key := range sourceFields {
			if _, ok := allowedSourceFields[strings.ToLower(strings.TrimSpace(key))]; !ok {
				return Event{}, fmt.Errorf("%w: source field %q is not approved", ErrSensitiveContent, key)
			}
		}
	}
	var event Event
	if err := json.Unmarshal(data, &event); err != nil {
		return Event{}, err
	}
	return event, nil
}

// ValidateOutbound checks the exact event value immediately before upload.
// It deliberately does not mutate the event: a rejected batch therefore has
// zero outbound bytes and does not alter local cursor/fingerprint state.
func ValidateOutbound(event Event) error {
	if strings.ContainsAny(event.Project, `/\\`) {
		return fmt.Errorf("%w: project must be a basename label", ErrSensitiveContent)
	}
	if strings.ContainsAny(event.Source.Identity, `/\\`) {
		return fmt.Errorf("%w: source identity contains a path", ErrSensitiveContent)
	}
	for name, value := range map[string]string{
		"event_id": event.EventID, "machine_id": event.MachineID, "session_id": event.SessionID,
		"provider": event.Provider, "model": event.Model, "canonical_model": event.CanonicalModel,
		"tool": event.Tool, "source.identity": event.Source.Identity, "source.adapter": event.Source.Adapter,
		"source.adapter_version": event.Source.AdapterVersion,
	} {
		if identifierContainsSensitiveValue(value) {
			return fmt.Errorf("%w: %s resembles a path or credential", ErrSensitiveContent, name)
		}
	}
	if err := validateApprovedMetadata(event.Metadata); err != nil {
		return err
	}
	return nil
}

func identifierContainsSensitiveValue(value string) bool {
	if value == "" {
		return false
	}
	if strings.ContainsAny(value, "\r\n") || strings.Contains(value, "Bearer ") || strings.Contains(value, "sk-") || strings.Contains(value, "ghp_") {
		return true
	}
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, `\\`) {
		return true
	}
	return len(value) >= 3 && value[1] == ':' && (value[2] == '/' || value[2] == '\\')
}

// ValidateOutboundBatch rejects the whole batch before the first HTTP
// request. The index is included for diagnostics; field values are not.
func ValidateOutboundBatch(events []Event) error {
	for index, event := range events {
		if err := ValidateOutbound(event); err != nil {
			return fmt.Errorf("event %d: %w", index, err)
		}
	}
	return nil
}

// FilterApprovedMetadata returns a copy containing only the explicitly
// namespaced metadata keys. Generic JSONL uses this to fail closed before it
// creates an event; native adapters currently emit no custom metadata.
func FilterApprovedMetadata(metadata map[string]any) (map[string]any, error) {
	if metadata == nil {
		return nil, nil
	}
	filtered := make(map[string]any, len(metadata))
	for key, value := range metadata {
		if !approvedMetadataKey(key) {
			return nil, fmt.Errorf("%w: metadata key %q is not approved", ErrSensitiveContent, key)
		}
		if err := validateMetadataValue(key, value); err != nil {
			return nil, err
		}
		filtered[key] = value
	}
	return filtered, nil
}

func validateApprovedMetadata(metadata map[string]any) error {
	_, err := FilterApprovedMetadata(metadata)
	return err
}

func approvedMetadataKey(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" || !strings.HasPrefix(strings.ToLower(key), metadataNamespace) {
		return false
	}
	for _, r := range key {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	lower := strings.ToLower(key)
	for _, sensitive := range []string{"prompt", "response", "content", "source", "path", "title", "secret", "credential", "token", "repository", "repo", "code", "message", "conversation", "command", "cwd", "url"} {
		if strings.Contains(lower, sensitive) {
			return false
		}
	}
	return len([]rune(key)) <= 128 && len(key) > len(metadataNamespace)
}

func validateMetadataValue(key string, value any) error {
	switch typed := value.(type) {
	case nil, bool, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return nil
	case string:
		lower := strings.ToLower(typed)
		if strings.ContainsAny(typed, "\r\n/\\") || strings.Contains(typed, "Bearer ") || strings.Contains(typed, "sk-") || strings.Contains(typed, "ghp_") {
			return fmt.Errorf("%w: metadata value for %q resembles sensitive content", ErrSensitiveContent, key)
		}
		for _, sensitive := range []string{"prompt", "response", "source", "path", "title", "secret", "credential", "repository", "repo", "code", "message", "conversation", "command", "cwd", "http:", "https:"} {
			if strings.Contains(lower, sensitive) {
				return fmt.Errorf("%w: metadata value for %q resembles sensitive content", ErrSensitiveContent, key)
			}
		}
		return nil
	case []any:
		for _, child := range typed {
			if err := validateMetadataValue(key, child); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		for childKey, child := range typed {
			if !approvedMetadataKey(key + "." + childKey) {
				return fmt.Errorf("%w: nested metadata key %q is not approved", ErrSensitiveContent, childKey)
			}
			if err := validateMetadataValue(key+"."+childKey, child); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: metadata value for %q has unsupported type", ErrSensitiveContent, key)
	}
}
