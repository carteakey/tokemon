package agent

import (
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/usage"
)

func TestDeltaTrackerSendsNewAndChangedEventsOnlyAfterSuccess(t *testing.T) {
	tracker := NewDeltaTracker()
	event := usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "event", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "provider", Model: "model", Tool: "tool", TotalTokens: usage.Int64(10), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}}

	if pending := tracker.Pending([]usage.Event{event}); len(pending) != 1 {
		t.Fatalf("initial pending events = %d, want 1", len(pending))
	}
	if pending := tracker.Pending([]usage.Event{event}); len(pending) != 1 {
		t.Fatalf("event was suppressed before successful upload")
	}
	tracker.MarkSent([]usage.Event{event})
	if pending := tracker.Pending([]usage.Event{event}); len(pending) != 0 {
		t.Fatalf("unchanged pending events = %d, want 0", len(pending))
	}

	event.TotalTokens = usage.Int64(11)
	if pending := tracker.Pending([]usage.Event{event}); len(pending) != 1 {
		t.Fatalf("changed pending events = %d, want 1", len(pending))
	}
	tracker.MarkSent([]usage.Event{event})
	if pending := tracker.Pending(nil); len(pending) != 0 {
		t.Fatalf("purged source produced pending events: %v", pending)
	}
}

func TestNewDeltaTrackerForcesVersionRestartBackfill(t *testing.T) {
	event := usage.Event{EventID: "event"}
	oldProcess := NewDeltaTracker()
	oldProcess.MarkSent([]usage.Event{event})
	newProcess := NewDeltaTracker()
	if pending := newProcess.Pending([]usage.Event{event}); len(pending) != 1 {
		t.Fatalf("restart pending events = %d, want full backfill", len(pending))
	}
}
