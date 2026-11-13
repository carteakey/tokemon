package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/usage"
)

// activityHeatmapEvent seeds one reported-accuracy event at midday UTC so the
// day bucket is unambiguous regardless of the machine's local timezone.
func activityHeatmapEvent(eventID string, day time.Time, tokens int64) usage.Event {
	return usage.Event{
		SchemaVersion: usage.SchemaVersion,
		EventID:       eventID,
		Timestamp:     time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, time.UTC),
		MachineID:     "machine-a",
		Provider:      "provider",
		Model:         "model",
		Tool:          "codex",
		TotalTokens:   usage.Int64(tokens),
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "codex", AdapterVersion: "test"},
	}
}

// activityGridCellRe matches one rendered heatmap cell tag (legend cells carry
// the class but not role="gridcell"; attributes never contain ">").
var activityGridCellRe = regexp.MustCompile(`<span class="activity-cell[^>]*>`)
var activityGridClassRe = regexp.MustCompile(`class="activity-cell ([^"]*)"`)

// activityGridCells returns the rendered gridcell tags of the dashboard page.
func activityGridCells(t *testing.T, body string) []string {
	t.Helper()
	var cells []string
	for _, tag := range activityGridCellRe.FindAllString(body, -1) {
		if strings.Contains(tag, `role="gridcell"`) {
			cells = append(cells, tag)
		}
	}
	return cells
}

func activityCellClass(tag string) string {
	parts := activityGridClassRe.FindStringSubmatch(tag)
	if parts == nil {
		return ""
	}
	return parts[1]
}

func activityCellIsUsage(tag string) bool {
	class := activityCellClass(tag)
	return regexp.MustCompile(`level-[1-4]`).MatchString(class) || strings.Contains(class, "unknown")
}

// The heatmap claims role="grid", so it must load with exactly one tabbable
// cell — the roving tab stop the arrow keys move — instead of one tab stop
// per active day.
func TestDashboardActivityGridRovingTabindex(t *testing.T) {
	t.Run("initial tab stop is the most recent active day", func(t *testing.T) {
		store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		today := time.Now().UTC()
		events := []usage.Event{
			activityHeatmapEvent("activity-tab-old", today.AddDate(0, 0, -10), 40),
			activityHeatmapEvent("activity-tab-recent", today.AddDate(0, 0, -3), 400),
		}
		if _, err := store.Ingest(context.Background(), events); err != nil {
			t.Fatal(err)
		}
		server, err := newTestServer(store, "")
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("dashboard status = %d, body %s", response.Code, response.Body.String())
		}

		cells := activityGridCells(t, response.Body.String())
		// The grid renders activityWeeks × 7 day cells; every one of them must
		// carry an explicit roving-tabindex decision.
		if len(cells) != 53*7 {
			t.Fatalf("activity grid renders %d gridcells, want 371", len(cells))
		}
		var usage []string
		tabStops := 0
		for _, tag := range cells {
			if strings.Contains(activityCellClass(tag), "future") {
				if strings.Contains(tag, "tabindex") {
					t.Fatal("a future cell is focusable; future cells stay aria-hidden and non-focusable")
				}
				if !strings.Contains(tag, `aria-hidden="true"`) {
					t.Fatal("a future cell lost aria-hidden")
				}
				continue
			}
			switch {
			case strings.Contains(tag, `tabindex="0"`):
				tabStops++
			case strings.Contains(tag, `tabindex="-1"`):
			default:
				t.Fatalf("non-future cell without a roving tabindex decision: %s", tag)
			}
			if activityCellIsUsage(tag) {
				usage = append(usage, tag)
			}
		}
		if tabStops != 1 {
			t.Fatalf("activity grid has %d tabindex=\"0\" cells, want exactly 1", tabStops)
		}
		if len(usage) == 0 {
			t.Fatal("seeded activity produced no usage cells")
		}
		// Document order is chronological, so the last usage cell is the most
		// recent active day: that is where the tab stop must sit.
		if !strings.Contains(usage[len(usage)-1], `tabindex="0"`) {
			t.Fatalf("the most recent active day is not the tab stop: %s", usage[len(usage)-1])
		}
	})

	t.Run("no activity falls back to the most recent non-future day", func(t *testing.T) {
		store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		server, err := newTestServer(store, "")
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("dashboard status = %d, body %s", response.Code, response.Body.String())
		}

		cells := activityGridCells(t, response.Body.String())
		var present []string
		for _, tag := range cells {
			if strings.Contains(activityCellClass(tag), "future") {
				continue
			}
			if strings.Contains(tag, `tabindex="0"`) {
				present = append(present, tag)
			}
		}
		if len(present) != 1 {
			t.Fatalf("empty activity grid has %d tabindex=\"0\" cells, want exactly 1", len(present))
		}
	})
}

func TestActivityInitialTab(t *testing.T) {
	day := func(date string, level int, future bool) database.ActivityDay {
		return database.ActivityDay{Date: date, Level: level, Future: future}
	}
	week := func(days ...database.ActivityDay) database.ActivityWeek {
		return database.ActivityWeek{Days: days}
	}
	// Two weeks, the last one half future: the grid always ends on the current
	// week, and days after today are future.
	activity := database.ActivityHeatmap{Weeks: []database.ActivityWeek{
		week(
			day("2026-11-01", 0, false), day("2026-11-02", 2, false), day("2026-11-03", 0, false),
			day("2026-11-04", 0, false), day("2026-11-05", 0, false), day("2026-11-06", 0, false),
			day("2026-11-07", 0, false),
		),
		week(
			day("2026-11-08", 0, false), day("2026-11-09", 1, false), day("2026-11-10", 0, false),
			day("2026-11-11", 3, false), day("2026-11-12", 0, false), day("2026-11-13", 0, true),
			day("2026-11-14", 0, true),
		),
	}}

	if got := activityInitialTab(activity); got != "2026-11-11" {
		t.Fatalf("activityInitialTab = %q, want the most recent active day 2026-11-11", got)
	}

	// With no active day at all the grid still gets one tab stop: the most
	// recent non-future day.
	quiet := database.ActivityHeatmap{Weeks: []database.ActivityWeek{
		week(
			day("2026-11-08", 0, false), day("2026-11-09", 0, false), day("2026-11-10", 0, false),
			day("2026-11-11", 0, false), day("2026-11-12", 0, false), day("2026-11-13", 0, true),
			day("2026-11-14", 0, true),
		),
	}}
	if got := activityInitialTab(quiet); got != "2026-11-12" {
		t.Fatalf("activityInitialTab without activity = %q, want the most recent non-future day 2026-11-12", got)
	}

	if got := activityTabStop(day("2026-11-11", 3, false), "2026-11-11"); got != "0" {
		t.Fatalf("activityTabStop on the initial day = %q, want 0", got)
	}
	if got := activityTabStop(day("2026-11-10", 0, false), "2026-11-11"); got != "-1" {
		t.Fatalf("activityTabStop off the initial day = %q, want -1", got)
	}
}
