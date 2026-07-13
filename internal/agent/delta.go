package agent

import (
	"crypto/sha256"
	"encoding/json"

	"github.com/tokemon/tokemon/internal/usage"
)

// DeltaTracker suppresses unchanged events during a running agent process.
// A restarted or upgraded agent intentionally starts empty and performs one
// complete compatibility backfill before returning to delta-only uploads.
type DeltaTracker struct {
	sent map[string][sha256.Size]byte
}

func NewDeltaTracker() *DeltaTracker {
	return &DeltaTracker{sent: make(map[string][sha256.Size]byte)}
}

func (t *DeltaTracker) Pending(events []usage.Event) []usage.Event {
	pending := make([]usage.Event, 0, len(events))
	for _, event := range events {
		fingerprint := eventFingerprint(event)
		if previous, ok := t.sent[event.EventID]; ok && previous == fingerprint {
			continue
		}
		pending = append(pending, event)
	}
	return pending
}

// MarkSent must only be called after the server accepts the batch.
func (t *DeltaTracker) MarkSent(events []usage.Event) {
	for _, event := range events {
		t.sent[event.EventID] = eventFingerprint(event)
	}
}

func eventFingerprint(event usage.Event) [sha256.Size]byte {
	payload, err := json.Marshal(event)
	if err != nil {
		// usage.Event contains only JSON-supported fields. Keep a stable fallback
		// so an unexpected encoder failure causes retries rather than data loss.
		return sha256.Sum256([]byte(event.EventID))
	}
	return sha256.Sum256(payload)
}
