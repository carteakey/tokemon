package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/evolution"
	insightai "github.com/tokemon/tokemon/internal/insights"
	"github.com/tokemon/tokemon/internal/quota"
	"github.com/tokemon/tokemon/internal/usage"
	"github.com/tokemon/tokemon/web"
)

type Server struct {
	store                *database.Store
	config               Config
	logger               *slog.Logger
	metrics              *Metrics
	csrfSecret           []byte
	sessionSecret        []byte
	trustedProxyNetworks []*net.IPNet
	loginThrottle        *loginThrottle
	template             *template.Template
	static               http.Handler
	assetVersion         string
	recapProvider        InsightsRecapProvider
	evolutionMu          sync.Mutex
	evolutionCache       *evolutionCacheEntry
	evolutionReady       chan struct{}
	evolutionGeneration  uint64
	evolutionRefreshes   atomic.Int64
	overviewMu           sync.Mutex
	overviewCache        *overviewCacheEntry
	overviewReady        chan struct{}
	overviewGeneration   uint64
	analyticsMu          sync.Mutex
	analyticsCache       map[string]*analyticsCacheEntry
	analyticsGeneration  uint64
	// analyticsRefreshes counts background analytics refreshes started since
	// process start. Tests assert on it to prove invalidation coalescing;
	// production code never reads it.
	analyticsRefreshes atomic.Int64
	catalogMu          sync.Mutex
	catalogCache       *catalogCacheEntry
	catalogReady       chan struct{}
	catalogGeneration  uint64
	catalogRefreshes   atomic.Int64
	sessionsMu         sync.Mutex
	sessionsCache      map[string]*sessionsCacheEntry
	sessionsGeneration uint64
	sessionsRefreshes  atomic.Int64
	insightsMu         sync.Mutex
	insightsCache      map[string]*insightsCacheEntry
	insightsGeneration uint64
	insightsRefreshes  atomic.Int64
	alerts             *quotaAlertState

	bgMu      sync.Mutex
	bgClosed  bool
	bgCtx     context.Context
	bgCancel  context.CancelFunc
	bgWG      sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

type evolutionCacheEntry struct {
	result     evolutionResponse
	err        error
	fetched    time.Time
	refreshDue time.Time
}

type catalogCacheEntry struct {
	result     database.CatalogSnapshot
	err        error
	fetched    time.Time
	refreshDue time.Time
}

type sessionsCacheEntry struct {
	query      database.AnalyticsQuery
	sort       string
	dir        string
	page       int
	pageSize   int
	result     database.SessionsPageResult
	err        error
	fetched    time.Time
	ready      chan struct{}
	hasResult  bool
	lastUse    time.Time
	refreshDue time.Time
}

type insightsCacheEntry struct {
	query      database.AnalyticsQuery
	result     database.Insights
	err        error
	fetched    time.Time
	ready      chan struct{}
	hasResult  bool
	lastUse    time.Time
	refreshDue time.Time
}

type overviewCacheEntry struct {
	result  database.Overview
	err     error
	fetched time.Time
	// html is the dashboard page rendered from result, or nil when the render
	// has not run or failed. Keeping it on the entry means a reader served this
	// overview is also served exactly the page built from it: the two can never
	// be stitched from different entries.
	html []byte
	etag string
}

// analyticsCacheEntry caches one filter combination's full Analytics result.
// ready is non-nil while a compute for the key is in flight, so concurrent
// requests for the same filter reuse one compute instead of stacking full
// scans on the store.
type analyticsCacheEntry struct {
	query   database.AnalyticsQuery
	result  database.Analytics
	err     error
	fetched time.Time
	ready   chan struct{}
	// hasResult marks an entry that has completed one compute, so stale-served
	// readers are distinguishable from a first-ever compute still in flight.
	hasResult bool
	// lastUse tracks the most recent serve for eviction: the cache is bounded,
	// and a filter that keeps being read must not be evicted ahead of one that
	// has gone quiet.
	lastUse time.Time
	// refreshDue is the earliest time an ingest-invalidated entry may start
	// its next refresh; zero means no invalidation is pending and
	// analyticsCacheTTL alone governs staleness. It is what coalesces a burst
	// of ingest invalidations into one refresh; see
	// analyticsMinRefreshInterval for the staleness it trades.
	refreshDue time.Time
}

type evolutionResponse struct {
	evolution.Snapshot
	Composition database.TokenComposition `json:"composition"`
}

//go:embed templates/*.html
var templateFS embed.FS

type PageChrome struct {
	Title       string
	ActiveNav   string
	CanLogout   bool
	CSRFToken   string
	ShowRefresh bool
}

func (s *Server) pageChrome(title, activeNav string, showRefresh bool) PageChrome {
	return PageChrome{
		Title:       title,
		ActiveNav:   activeNav,
		CanLogout:   s.passwordAccountConfigured(),
		CSRFToken:   s.csrfToken(),
		ShowRefresh: showRefresh,
	}
}

type dashboardPage struct {
	PageChrome
	database.Overview
	ModelAliases   map[string]string
	MachineAliases map[string]string
	Machines       []database.MachineInfo
	Sessions       []database.SessionRecord
	Freshness      dataFreshness
	Quotas         []quotaView
}

// dataFreshness describes how recent the newest persisted event is, using the
// same pill vocabulary as machine health. It answers the "is my day total low
// because a connector is down?" question from persisted data alone.
type dataFreshness struct {
	Class string
	State string
	Age   string
}

func dataFreshnessFor(newest *time.Time, now time.Time) dataFreshness {
	if newest == nil {
		return dataFreshness{Class: "", State: "Unknown", Age: "no events yet"}
	}
	age := now.Sub(*newest)
	if age < 0 {
		age = 0
	}
	if age <= 30*time.Minute {
		return dataFreshness{Class: "connected", State: "Fresh", Age: durationLabel(age.Milliseconds()) + " ago"}
	}
	return dataFreshness{Class: "stale", State: "Stale", Age: durationLabel(age.Milliseconds()) + " ago"}
}

type analyticsPageData struct {
	PageChrome
	database.Analytics
	ModelAliases      map[string]string
	MachineAliases    map[string]string
	ExportURL         string
	CSVURL            string
	PeakPoint         database.AnalyticsPoint
	AverageThread     int64
	AxisMax           int64
	AxisTicks         []analyticsAxisTick
	Insights          []analyticsInsight
	Quotas            []quotaView
	ActiveFilters     []activeFilterPill
	ActiveFilterCount int
}

type analyticsAxisTick struct {
	Value    int64
	Position int
}

// InsightsRecapStatus describes the optional AI recap slot on the Insights
// page. The dashboard itself remains useful without a configured provider.
type InsightsRecapStatus string

const (
	InsightsRecapGenerated   InsightsRecapStatus = "generated"
	InsightsRecapDisabled    InsightsRecapStatus = "disabled"
	InsightsRecapUnavailable InsightsRecapStatus = "unavailable"
)

// InsightsRecap is deliberately small so a future AI adapter can be injected
// without changing the page contract. It contains a recap only, never source
// records or prompts.
type InsightsRecap struct {
	Status InsightsRecapStatus `json:"status"`
	Text   string              `json:"text,omitempty"`
}

// InsightsRecapProvider is an optional API-local seam for a future AI recap
// integration. Implementations should summarize aggregate Insights data only.
type InsightsRecapProvider interface {
	Generate(context.Context, database.Insights) (string, error)
}

type aggregateAIRecapProvider struct {
	synthesizer insightai.Synthesizer
}

func (provider aggregateAIRecapProvider) Generate(ctx context.Context, result database.Insights) (string, error) {
	output, err := provider.synthesizer.Synthesize(ctx, aggregateAIInput(result))
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(output.Title) == "" {
		return strings.TrimSpace(output.Summary), nil
	}
	return strings.TrimSpace(output.Title) + " — " + strings.TrimSpace(output.Summary), nil
}

func aggregateAIInput(result database.Insights) insightai.Input {
	scope := insightai.ScopeOverall
	if result.Filter.Machine != "" || result.Filter.Provider != "" || result.Filter.Model != "" || result.Filter.Tool != "" {
		scope = insightai.ScopeFilteredView
	}
	evidence := make([]insightai.Evidence, 0, len(result.Cards))
	for _, card := range result.Cards {
		observation := card.Observation
		basis := card.Basis
		if card.Category == "concentration" {
			observation = "One selected-dimension category has the largest token share; its label stays local."
			basis = "largest selected-dimension token share / window token total; category labels are withheld"
		}
		evidence = append(evidence, insightai.Evidence{
			ID:          card.ID,
			Category:    card.Category,
			Observation: observation,
			Basis:       basis,
		})
	}
	return insightai.Input{
		PeriodLabel: result.Period,
		Timezone:    result.Timezone,
		ScopeLabels: []insightai.ScopeLabel{{Label: scope, Count: len(result.Cards)}},
		DataQuality: insightai.DataQuality{Level: result.DataQuality.Confidence, Note: result.DataQuality.Qualifier},
		Evidence:    evidence,
	}
}

type insightsPageData struct {
	PageChrome
	database.Insights
	Facets         database.AnalyticsFacets
	ModelAliases   map[string]string
	MachineAliases map[string]string
	HourlyMax      int64
	WeekdayMax     int64
	Recap          InsightsRecap
}

type aliasRow struct {
	Identity      string
	Alias         string
	Default       string
	Tokens        int64
	Events        int64
	UnknownEvents int64
	HasUsage      bool
	AnalyticsURL  string
	TokedexURL    string
}

type settingsQuotaRow struct {
	Provider      string
	Model         string
	Label         string
	DisplayLabel  string
	ScopeLabel    string
	ResetDay      int
	TokensValue   string
	CostValue     string
	Source        string
	SourceBadge   string
	OverridesYAML bool
	CanDelete     bool
	DeleteLabel   string
	SaveLabel     string
	StatusFigures string
	StatusDetail  string
	StatusPercent float64
	StatusFill    float64
	StatusOver    bool
	HasStatus     bool
}

type settingsBackupInfo struct {
	StatusLabel string
	AgeLabel    string
	TimestampUI template.HTML
	Count       int
	Schedule    string
	Exists      bool
	Error       bool
}

type settingsPage struct {
	PageChrome
	Models      []aliasRow
	Machines    []aliasRow
	Quotas      []settingsQuotaRow
	Backup      settingsBackupInfo
	Saved       bool
	QuotaNotice string
	Error       string
	AuthMode    string
	TokenSource string
}

type tokedexWhatIfRow struct {
	ID                 string
	DisplayName        string
	Provider           string
	Tier               string
	Priced             bool
	InputRate          *float64
	CacheReadRate      *float64
	OutputRate         *float64
	InputEff           float64
	CacheReadEff       float64
	CacheWriteEff      float64
	OutputEff          float64
	InputRateAttr      string
	CacheReadRateAttr  string
	CacheWriteRateAttr string
	OutputRateAttr     string
	Cost               float64
	CostLabel          string
	Note               string
	DetailURL          string
}

type catalogTier struct {
	Name   string
	Models []database.CatalogEntry
}

// catalogTierInfo is one legend row: a tier letter and what it means.
type catalogTierInfo struct {
	Name        string
	Description string
}

// catalogTierDescriptions explains the bundled tier letters. Tiers are an
// editorial grouping in catalog/models.yaml, not a measurement of quality,
// and the legend copy says so explicitly so the page cannot imply otherwise.
var catalogTierDescriptions = map[string]string{
	"S":        "Frontier capability — the strongest fit for hard, long-running work.",
	"A":        "Strong general-purpose models for most coding and analysis work.",
	"B":        "Balanced everyday models; cheaper defaults for routine tasks.",
	"C":        "Lightweight models for high-volume or simple jobs.",
	"D":        "Minimal models; lowest cost, narrow use.",
	"Unranked": "Not yet tiered; the catalog has no editorial ranking for this family.",
}

// catalogTierLegend builds one row per tier that actually has models, in the
// catalog's tier order. A legend for an empty tier would be noise.
func catalogTierLegend(tiers []catalogTier) []catalogTierInfo {
	legend := make([]catalogTierInfo, 0, len(tiers))
	for _, tier := range tiers {
		description, ok := catalogTierDescriptions[tier.Name]
		if !ok {
			description = "Editorial tier from the bundled catalog."
		}
		legend = append(legend, catalogTierInfo{Name: tier.Name, Description: description})
	}
	return legend
}

// catalogProviders lists the distinct providers present in the rendered
// tiers, sorted so the client-side provider filter keeps a stable order.
func catalogProviders(tiers []catalogTier) []string {
	seen := make(map[string]bool)
	providers := make([]string, 0, len(tiers))
	for _, tier := range tiers {
		for _, model := range tier.Models {
			if model.Provider == "" || seen[model.Provider] {
				continue
			}
			seen[model.Provider] = true
			providers = append(providers, model.Provider)
		}
	}
	sort.Strings(providers)
	return providers
}

type catalogPageData struct {
	PageChrome
	Tiers                  []catalogTier
	TierLegend             []catalogTierInfo
	Providers              []string
	ModelCount             int
	Uncataloged            []database.UncatalogedModel
	ObservedTokens         int64
	PricedTokens           int64
	CatalogedUnknownTokens int64
	UncatalogedModels      int
	Recent30d              database.ComponentUsageTotals
	WhatIfRows             []tokedexWhatIfRow
	WhatIfByID             map[string]tokedexWhatIfRow
	WhatIfHasUsage         bool
}

type tokedexDetailPageData struct {
	PageChrome
	Detail          database.TokedexModelDetail
	ModelAliases    map[string]string
	MachineAliases  map[string]string
	AnalyticsURL    string
	SessionsURL     string
	WeeklyMaxTokens int64
}

type activeFilterPill struct {
	Label     string
	RemoveURL string
}

type sessionsPageData struct {
	PageChrome
	Sessions       []database.SessionRecord
	Filter         database.AnalyticsQuery
	Facets         database.AnalyticsFacets
	ModelAliases   map[string]string
	MachineAliases map[string]string
	Sorts          map[string]sessionSortState
	Sort           string
	Dir            string
	Page           int
	PageSize       int
	TotalCount     int64
	TotalPages     int
	HasPrev        bool
	HasNext        bool
	RangeStart     int
	RangeEnd       int
	ActiveFilters  []activeFilterPill
	PrevURL        string
	NextURL        string
}

type sessionDetailPageData struct {
	PageChrome
	Detail         database.SessionDetail
	ModelAliases   map[string]string
	MachineAliases map[string]string
}

// sessionSortState carries the per-column sortable header state: the toggle
// URL, whether this column is the active sort, and its aria-sort direction.
type sessionSortState struct {
	URL       string
	Active    bool
	AriaSort  string
	Indicator string
}

const dashboardUsageRowsLimit = 5

const (
	glyphOff uint8 = iota
	glyphDim
	glyphOn
)

type pixelGlyph struct {
	Cells   [25]uint8
	Palette int
	Kind    string
	Preset  string
}

func glyphSeed(kind, name string) uint32 {
	normalized := kind + ":" + strings.ToLower(strings.TrimSpace(name))
	if strings.HasSuffix(normalized, ":") {
		normalized += "unknown"
	}
	seed := uint32(2166136261)
	for index := 0; index < len(normalized); index++ {
		seed ^= uint32(normalized[index])
		seed *= 16777619
	}
	return seed
}

func nextGlyphSeed(state uint32) uint32 {
	state ^= state << 13
	state ^= state >> 17
	state ^= state << 5
	return state
}

func glyphFromRows(kind, preset string, palette int, rows ...string) pixelGlyph {
	glyph := pixelGlyph{Kind: kind, Preset: preset, Palette: palette}
	for row, pattern := range rows {
		if row >= 5 {
			break
		}
		for column := 0; column < len(pattern) && column < 5; column++ {
			switch pattern[column] {
			case 'd':
				glyph.Cells[row*5+column] = glyphDim
			case 'o':
				glyph.Cells[row*5+column] = glyphOn
			}
		}
	}
	return glyph
}

func glyphCellClass(state uint8) string {
	switch state {
	case glyphDim:
		return "dim"
	case glyphOn:
		return "on"
	default:
		return "off"
	}
}

func glyphForModel(name string) pixelGlyph {
	seed := glyphSeed("model", name)
	glyph := pixelGlyph{Kind: "model", Preset: "generated", Palette: int(seed % 4)}
	for index := range glyph.Cells {
		glyph.Cells[index] = glyphDim
	}
	state := seed
	for row := 0; row < 5; row++ {
		state = nextGlyphSeed(state)
		for column := 0; column < 3; column++ {
			if state&(1<<column) != 0 {
				glyph.Cells[row*5+column] = glyphOn
				glyph.Cells[row*5+(4-column)] = glyphOn
			}
		}
	}
	glyph.Cells[12] = glyphOn
	return glyph
}

func glyphForProject(name string) pixelGlyph {
	seed := glyphSeed("project", name)
	glyph := pixelGlyph{Kind: "project", Preset: "generated", Palette: int(seed % 4)}
	for column := 0; column < 3; column++ {
		glyph.Cells[column] = glyphDim
	}
	for column := 0; column < 5; column++ {
		glyph.Cells[5+column] = glyphDim
		glyph.Cells[20+column] = glyphDim
	}
	state := seed
	for row := 2; row < 4; row++ {
		glyph.Cells[row*5] = glyphDim
		glyph.Cells[row*5+4] = glyphDim
		state = nextGlyphSeed(state)
		for column := 1; column < 4; column++ {
			glyph.Cells[row*5+column] = glyphDim
			if state&(1<<column) != 0 {
				glyph.Cells[row*5+column] = glyphOn
			}
		}
	}
	glyph.Cells[1] = glyphOn
	return glyph
}

func glyphForMachine(name string) pixelGlyph {
	seed := glyphSeed("machine", name)
	glyph := pixelGlyph{Kind: "machine", Preset: "generated", Palette: int(seed % 4)}
	for _, row := range []int{0, 2, 4} {
		for column := 0; column < 5; column++ {
			glyph.Cells[row*5+column] = glyphDim
		}
	}
	state := seed
	for _, row := range []int{1, 3} {
		glyph.Cells[row*5] = glyphDim
		glyph.Cells[row*5+4] = glyphDim
		state = nextGlyphSeed(state)
		for column := 1; column < 4; column++ {
			glyph.Cells[row*5+column] = glyphDim
			if state&(1<<column) != 0 {
				glyph.Cells[row*5+column] = glyphOn
			}
		}
	}
	glyph.Cells[8] = glyphOn
	glyph.Cells[18] = glyphOn
	return glyph
}

func glyphForHarness(tool string) pixelGlyph {
	switch strings.ToLower(strings.TrimSpace(tool)) {
	case "codex":
		return glyphFromRows("harness", "codex", 1, ".odo.", "od.do", "d.o.d", "od.do", ".odo.")
	case "claude", "claude-code":
		return glyphFromRows("harness", "claude", 3, "o.d.o", ".ooo.", "doood", ".ooo.", "o.d.o")
	case "claude-desktop":
		return glyphFromRows("harness", "claude-desktop", 3, "ooooo", "o.d.o", "doood", "o.d.o", "ooooo")
	case "opencode":
		return glyphFromRows("harness", "opencode", 2, "ddddd", "o...d", "d.ood", "d...o", "ddddd")
	case "copilot-cli", "github-copilot":
		return glyphFromRows("harness", "copilot", 3, "d...d", ".ddd.", "ddddd", ".ddd.", "d...d")
	case "antigravity", "gemini":
		return glyphFromRows("harness", "gemini", 0, "..o..", ".odo.", "ododo", ".odo.", "..o..")
	case "openclaw":
		return glyphFromRows("harness", "openclaw", 1, ".ooo.", "o...o", "ooooo", "o...o", ".ooo.")
	case "hermes-agent", "hermes":
		return glyphFromRows("harness", "hermes", 2, "..o..", ".odo.", "oodoo", ".odo.", "o...o")
	case "zcode":
		return glyphFromRows("harness", "zcode", 2, "ddddd", "..o..", ".d...", ".o...", "ddddd")
	case "grok", "xai":
		return glyphFromRows("harness", "grok", 1, "o...o", ".o.o.", "..o..", ".o.o.", "o...o")
	case "deepseek":
		return glyphFromRows("harness", "deepseek", 1, "ddddd", "...oo", "..oo.", ".oo..", "oo...")
	case "generic-jsonl":
		return glyphFromRows("harness", "generic", 2, ".ddd.", ".dod.", ".dod.", ".ddd.", "..o..")
	default:
		glyph := glyphForModel("harness:" + tool)
		glyph.Kind = "harness"
		return glyph
	}
}

func topProjects(values []database.ProjectTotal) []database.ProjectTotal {
	return limitDashboardRows(values)
}

func topModels(values []database.ModelTotal) []database.ModelTotal {
	return limitDashboardRows(values)
}

func topTools(values []database.ToolTotal) []database.ToolTotal {
	return limitDashboardRows(values)
}

func harnessName(tool string) string {
	switch strings.ToLower(strings.TrimSpace(tool)) {
	case "claude-code":
		return "Claude Code"
	case "claude-desktop":
		return "Claude Desktop"
	case "codex":
		return "Codex"
	case "opencode":
		return "OpenCode"
	case "copilot-cli", "github-copilot":
		return "GitHub Copilot CLI"
	case "antigravity":
		return "Antigravity"
	case "openclaw":
		return "OpenClaw"
	case "hermes-agent", "hermes":
		return "Hermes Agent"
	case "zcode":
		return "ZCode"
	case "generic-jsonl":
		return "Generic JSONL"
	default:
		return tool
	}
}

// sortableHeader renders one sortable session-table header cell. The cell
// links to the toggled sort, exposes aria-sort when active, and marks the
// direction with a text arrow hidden from assistive tech.
func sortableHeader(states map[string]sessionSortState, key string) template.HTML {
	state, ok := states[key]
	if !ok {
		return ""
	}
	labels := map[string]string{"when": "When", "tokens": "Total", "cost": "Cost", "duration": "Duration"}
	attr := ""
	if state.Active {
		attr = ` aria-sort="` + state.AriaSort + `"`
	}
	arrow := ""
	if state.Indicator != "" {
		arrow = ` <span aria-hidden="true">` + state.Indicator + `</span>`
	}
	return template.HTML(`<th scope="col"` + attr + `><a class="session-sort" href="` + template.HTMLEscapeString(state.URL) + `">` + labels[key] + arrow + `</a></th>`)
}

// sessionDisplayName renders a raw session ID for the dashboard tables.
// Provider IDs arrive in a handful of noisy shapes (sess_<uuid>,
// sess_subagent_agent_<uuid>, session_<hex>, local-<hex>, sha256:<hex>,
// YYYYMMDD_HHMMSS_<hex>); the display name strips the known noise prefixes and
// keeps a short stable identifier, expanding only the timestamp shape that
// carries real meaning. The full ID stays available in the cell's title
// attribute. Empty IDs render as empty and unknown shapes pass through
// truncated.
func sessionDisplayName(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if display, ok := sessionTimestampName(trimmed); ok {
		return display
	}
	for _, prefix := range []string{"sha256:", "local-", "sess_subagent_agent_", "sess_", "ses_", "session_"} {
		if rest, ok := strings.CutPrefix(trimmed, prefix); ok && rest != "" {
			label := "session"
			if strings.HasPrefix(prefix, "sess_subagent") {
				label = "subagent"
			}
			return label + " " + shortID(rest)
		}
	}
	if len(trimmed) > 18 {
		return trimmed[:18] + "…"
	}
	return trimmed
}

func shortID(value string) string {
	value = strings.TrimFunc(value, func(r rune) bool {
		return !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'))
	})
	if len(value) > 8 {
		return value[:8]
	}
	return value
}

// copyIDButton renders the click-to-copy affordance for a full session or
// model ID. Display names stay readable while the full ID — otherwise only
// reachable through the hover-only title attribute — becomes a one-click
// copy via the page's delegated copy-id handler.
func copyIDButton(value, kind string) template.HTML {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	label := "Copy full " + kind + " ID"
	return template.HTML(`<button type="button" class="copy-id" data-copy-value="` + template.HTMLEscapeString(trimmed) + `" title="` + label + `" aria-label="` + label + `"><span class="copy-id-text">Copy ID</span></button>`)
}

// copyIDScript is the delegated click-to-copy handler for pages that render
// copyIDButton. It prefers the async clipboard API with an execCommand
// fallback, flips the clicked button to a brief "Copied" state, and survives
// server-driven row updates because it listens on the document.
const copyIDScript = `<script>
(() => {
  'use strict';
  const RESET_MS = 1600;
  const markCopied = (button) => {
    const label = button.querySelector('.copy-id-text');
    button.classList.add('copied');
    if (label) label.textContent = 'Copied';
    setTimeout(() => {
      button.classList.remove('copied');
      if (label) label.textContent = 'Copy ID';
    }, RESET_MS);
  };
  const legacyCopy = (value) => {
    const field = document.createElement('textarea');
    field.value = value;
    field.setAttribute('readonly', '');
    field.setAttribute('aria-hidden', 'true');
    field.style.position = 'fixed';
    field.style.opacity = '0';
    document.body.append(field);
    field.select();
    let copied = false;
    try { copied = document.execCommand('copy'); } catch { copied = false; }
    field.remove();
    return copied;
  };
  document.addEventListener('click', (event) => {
    const button = event.target instanceof Element ? event.target.closest('.copy-id') : null;
    if (!button) return;
    const value = button.getAttribute('data-copy-value') || '';
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(value).then(() => markCopied(button), () => { if (legacyCopy(value)) markCopied(button); });
    } else if (legacyCopy(value)) {
      markCopied(button);
    }
  });
})();
</script>`

// sessionTimestampName expands the hermes YYYYMMDD_HHMMSS_<hex> shape, the only
// raw session ID that carries human-readable time information.
func sessionTimestampName(raw string) (string, bool) {
	if len(raw) < 16 || raw[8] != '_' || raw[15] != '_' {
		return "", false
	}
	for _, r := range raw[:8] {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	for _, r := range raw[9:15] {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return raw[0:4] + "-" + raw[4:6] + "-" + raw[6:8] + " " + raw[9:11] + ":" + raw[11:13], true
}

func compactModelName(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return raw
	}
	parts := strings.FieldsFunc(trimmed, func(r rune) bool { return r == '-' || r == '_' || r == '/' })
	if len(parts) < 2 {
		return trimmed
	}
	if strings.EqualFold(parts[0], "claude") {
		parts = parts[1:]
		if len(parts) > 1 && len(parts[len(parts)-1]) == 8 && isDigits(parts[len(parts)-1]) {
			parts = parts[:len(parts)-1]
		}
		if len(parts) == 0 {
			return trimmed
		}
		// Adjacent numeric parts are one version: claude-opus-4-5 is Opus 4.5.
		words := make([]string, 0, len(parts))
		for index, part := range parts {
			if index > 0 && isDigits(part) && isDigits(parts[index-1]) {
				words[len(words)-1] += "." + part
				continue
			}
			words = append(words, titleWord(part))
		}
		return strings.Join(words, " ")
	}
	if strings.EqualFold(parts[0], "gpt") {
		if len(parts) == 2 {
			return "GPT-" + parts[1]
		}
		return "GPT-" + parts[1] + " " + strings.Join(titleWords(parts[2:]), " ")
	}
	return trimmed
}

func titleWords(values []string) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = titleWord(value)
	}
	return result
}

func compactMachineName(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return raw
	}
	parts := strings.FieldsFunc(trimmed, func(r rune) bool { return r == '-' || r == '_' || r == ' ' })
	for index, part := range parts {
		if strings.EqualFold(part, "macbook") {
			words := titleWords(parts[index:])
			words[0] = "MacBook"
			return strings.Join(words, " ")
		}
	}
	if len(parts) == 2 && len(parts[0]) <= 4 {
		return titleWord(parts[1])
	}
	return trimmed
}

func titleWord(value string) string {
	if value == "" {
		return value
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func isDigits(value string) bool {
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func modelDisplayName(aliases map[string]string, raw string) string {
	if alias := strings.TrimSpace(aliases[raw]); alias != "" {
		return alias
	}
	// The Claude adapter emits the literal "unknown" when a record's model
	// cannot be parsed. It is not a real model: label the aggregate row as
	// unknown instead of presenting it alongside catalog models.
	if strings.EqualFold(raw, "unknown") {
		return "Unknown model"
	}
	return compactModelName(raw)
}

func machineDisplayName(aliases map[string]string, raw string) string {
	if alias := strings.TrimSpace(aliases[raw]); alias != "" {
		return alias
	}
	return compactMachineName(raw)
}

func analyticsBarPercent(value, maximum int64) float64 {
	if value <= 0 || maximum <= 0 {
		return 0
	}
	return float64(value) * 100 / float64(maximum)
}

func analyticsPartPercent(value, total int64) float64 {
	if value <= 0 || total <= 0 {
		return 0
	}
	return float64(value) * 100 / float64(total)
}

func analyticsNiceMax(maximum int64) int64 {
	if maximum <= 0 {
		return 0
	}
	power := math.Pow(10, math.Floor(math.Log10(float64(maximum))))
	fraction := float64(maximum) / power
	niceFraction := 10.0
	for _, candidate := range []float64{1, 1.25, 1.5, 2, 2.5, 5, 10} {
		if fraction <= candidate {
			niceFraction = candidate
			break
		}
	}
	return int64(math.Ceil(niceFraction * power))
}

func analyticsAxisTicks(maximum int64) []analyticsAxisTick {
	if maximum <= 0 {
		return nil
	}
	ticks := make([]analyticsAxisTick, 0, 5)
	for position := 100; position >= 0; position -= 25 {
		ticks = append(ticks, analyticsAxisTick{
			Value:    int64(math.Round(float64(maximum) * float64(position) / 100)),
			Position: position,
		})
	}
	return ticks
}

func analyticsDateTickClass(index, total int, period string) string {
	if total <= 0 || index < 0 || index >= total {
		return ""
	}
	step := 1
	switch period {
	case "24h":
		step = 4
	case "30d":
		step = 5
	case "90d":
		step = 15
	case "all":
		step = max(1, int(math.Ceil(float64(total)/6)))
	}
	if index != 0 && index != total-1 && index%step != 0 {
		return ""
	}
	class := " tick"
	if index == 0 {
		class += " tick-start"
	}
	if index == total-1 {
		class += " tick-end"
	}
	return class
}

func analyticsPointDateRange(point database.AnalyticsPoint, bucket string, loc *time.Location) (date, from, to string) {
	if loc == nil {
		loc = time.UTC
	}
	switch bucket {
	case "hour":
		parsed, err := time.Parse(time.RFC3339, point.Date)
		if err != nil {
			return "", "", ""
		}
		day := parsed.In(loc).Format("2006-01-02")
		return day, day, day
	case "month":
		parsed, err := time.Parse("2006-01-02", point.Date)
		if err != nil {
			return "", "", ""
		}
		fromDay := parsed.Format("2006-01-02")
		toDay := parsed.AddDate(0, 1, -1).Format("2006-01-02")
		return fromDay, fromDay, toDay
	default:
		if len(point.Date) != 10 {
			return "", "", ""
		}
		return point.Date, point.Date, point.Date
	}
}

func analyticsDateTickLabel(point database.AnalyticsPoint, period string) string {
	if period != "24h" {
		return point.Label
	}
	parts := strings.Fields(point.Label)
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return point.Label
}

func analyticsShareDateTickLabel(point database.AnalyticsSharePoint, period string) string {
	if period != "24h" {
		return point.Label
	}
	parts := strings.Fields(point.Label)
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return point.Label
}

func analyticsTickPeriod(bucket string, count int) string {
	switch bucket {
	case "hour":
		return "24h"
	case "month":
		return "all"
	default:
		if count <= 10 {
			return "7d"
		}
		if count <= 35 {
			return "30d"
		}
		return "90d"
	}
}

func analyticsDimensionLabel(dimension string) string {
	switch dimension {
	case "harnesses":
		return "Harnesses"
	case "providers":
		return "Providers"
	case "models":
		return "Models"
	case "machines":
		return "Machines"
	default:
		return "Projects"
	}
}

// analyticsUnknownLabel is the dimension-aware display label for the empty
// breakdown bucket. The store COALESCEs every dimension to the raw sentinel
// 'Unknown' (kept verbatim in the DB expression and JSON API); the overview
// spells the concept out per dimension ("Unknown project"), so the analytics
// page and CSV do the same at display time.
func analyticsUnknownLabel(dimension string) string {
	switch dimension {
	case "harnesses":
		return "Unknown harness"
	case "providers":
		return "Unknown provider"
	case "models":
		return "Unknown model"
	case "machines":
		return "Unknown machine"
	default:
		return "Unknown project"
	}
}

// isUnknownBucketName reports whether a breakdown bucket name is the empty
// bucket sentinel. The store writes 'Unknown', while model rows can also
// carry the Claude adapter's literal "unknown"; both spell the same concept.
func isUnknownBucketName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), "unknown")
}

// breakdownDisplayName renders one breakdown/share name for display: the
// empty bucket becomes the dimension-aware unknown label and real names keep
// their per-dimension formatting (model/machine aliases, harness names).
func breakdownDisplayName(dimension string, modelAliases, machineAliases map[string]string, name string) string {
	if isUnknownBucketName(name) {
		return analyticsUnknownLabel(dimension)
	}
	switch dimension {
	case "models":
		return modelDisplayName(modelAliases, name)
	case "machines":
		return machineDisplayName(machineAliases, name)
	case "harnesses":
		return harnessName(name)
	default:
		return name
	}
}

func analyticsPeakPoint(points []database.AnalyticsPoint) database.AnalyticsPoint {
	if len(points) == 0 {
		return database.AnalyticsPoint{}
	}
	peak := points[0]
	for _, point := range points[1:] {
		if point.Tokens > peak.Tokens {
			peak = point
		}
	}
	return peak
}

func analyticsAverageThread(summary database.AnalyticsSummary) int64 {
	if summary.Threads <= 0 {
		return 0
	}
	return summary.SessionTokens / summary.Threads
}

// analyticsInsight is one compact usage-analysis card for the analytics
// page's Insights strip. Everything is derived from the Analytics result the
// page already loaded — no extra queries — and missing inputs render with the
// dashboard's Unknown vocabulary. State feeds the shared comparison pill
// classes (up/down/flat; empty is neutral or unknown).
type analyticsInsight struct {
	ID        string
	Label     string
	Value     string
	State     string
	Detail    string
	LinkURL   string
	LinkLabel string
}

// analyticsMoverShareThreshold keeps the mover card honest: sub-0.1pt share
// drift across a window is noise, not a trend.
const analyticsMoverShareThreshold = 0.1

func analyticsInsights(result database.Analytics, now time.Time, modelAliases, machineAliases map[string]string) []analyticsInsight {
	return analyticsInsightsWithTokedex(result, now, modelAliases, machineAliases, false)
}

func analyticsInsightsWithTokedex(result database.Analytics, now time.Time, modelAliases, machineAliases map[string]string, tokedex bool) []analyticsInsight {
	var insights []analyticsInsight

	// Spend burn rate: the window's daily cost average and, for day-bucketed
	// windows, what the current month totals at that pace. Hour buckets and
	// unpriced-only windows have no honest rate, so they stay Unknown.
	if result.Bucket == "day" && len(result.Points) > 0 {
		daily := result.Summary.EstimatedCost.Amount / float64(len(result.Points))
		if result.Summary.EstimatedCost.PricedTokens > 0 {
			year, month, _ := now.Date()
			daysInMonth := time.Date(year, month+1, 0, 0, 0, 0, 0, now.Location()).Day()
			projected := daily * float64(daysInMonth)
			insights = append(insights, analyticsInsight{
				ID:    "burn_rate",
				Label: "Spend burn rate",
				Value: money(daily) + "/day",
				Detail: fmt.Sprintf("At this window's pace, %s lands around %s.",
					now.Format("January"), money(projected)),
			})
		} else {
			insights = append(insights, analyticsInsight{
				ID:    "burn_rate",
				Label: "Spend burn rate",
				Value: "Unknown",
				Detail: fmt.Sprintf("No priced usage in this window%s.",
					unpricedSuffix(result.Summary.EstimatedCost.UnpricedTokens)),
			})
		}
	}

	// Top mover: the series whose share of the window moved most between its
	// first and second half, so a shifting mix is visible without reading the
	// share chart. Single-bucket windows have no halves and are omitted.
	if len(result.SharePoints) >= 2 {
		mid := len(result.SharePoints) / 2
		best := -1
		var bestDelta, bestFirst, bestSecond float64
		for index := range result.ShareSeries {
			if result.ShareSeries[index].Name == "Other" {
				continue
			}
			var first, second float64
			for _, point := range result.SharePoints[:mid] {
				first += point.Values[index].Share
			}
			for _, point := range result.SharePoints[mid:] {
				second += point.Values[index].Share
			}
			first /= float64(max(mid, 1))
			second /= float64(max(len(result.SharePoints)-mid, 1))
			if delta := (second - first) * 100; best == -1 || abs64(delta) > abs64(bestDelta) {
				best, bestDelta, bestFirst, bestSecond = index, delta, first, second
			}
		}
		if best >= 0 && abs64(bestDelta) >= analyticsMoverShareThreshold {
			name := result.ShareSeries[best].Name
			detailName := name
			switch result.Filter.Dimension {
			case "models":
				detailName = modelDisplayName(modelAliases, name)
			case "machines":
				detailName = machineDisplayName(machineAliases, name)
			}
			direction := "down"
			if bestDelta > 0 {
				direction = "up"
			}
			insights = append(insights, analyticsInsight{
				ID:    "top_mover",
				Label: "Top mover",
				Value: fmt.Sprintf("%+.1f pts", bestDelta),
				State: direction,
				Detail: fmt.Sprintf("%s held %.0f%% of the window's first half and %.0f%% of the second half.",
					detailName, bestFirst*100, bestSecond*100),
			})
		}
	}

	// Cache efficiency: this window's hit rate with the previous window's as
	// the baseline. The previous rate rides along in Comparison at no extra
	// query cost.
	if result.Summary.Cache.EligibleTokens > 0 {
		current := result.Summary.Cache.HitRate * 100
		insight := analyticsInsight{
			ID:     "cache_efficiency",
			Label:  "Cache efficiency",
			Value:  fmt.Sprintf("%.1f%%", current),
			Detail: fmt.Sprintf("%s of eligible input tokens were served from cache.", commas(result.Summary.Cache.CachedTokens)),
		}
		if result.Comparison != nil && result.Comparison.PreviousCacheHitRate != nil {
			delta := current - *result.Comparison.PreviousCacheHitRate*100
			insight.State = "flat"
			if delta > 0 {
				insight.State = "up"
			} else if delta < 0 {
				insight.State = "down"
			}
			insight.Detail = fmt.Sprintf("%s of eligible input tokens were served from cache, %+.1f pts vs the previous window.",
				commas(result.Summary.Cache.CachedTokens), delta)
		}
		insights = append(insights, insight)
	} else {
		insights = append(insights, analyticsInsight{
			ID:     "cache_efficiency",
			Label:  "Cache efficiency",
			Value:  "Unknown",
			Detail: "No cacheable input tokens in this window.",
		})
	}

	// Catalog coverage: the share of window tokens without pricing, the same
	// gap the Tokedex coverage line reports. Directionless on purpose — the
	// fix is adding catalog entries, not a trend line — and omitted when the
	// window is fully priced.
	total := result.Summary.EstimatedCost.PricedTokens + result.Summary.EstimatedCost.UnpricedTokens
	if result.Summary.EstimatedCost.UnpricedTokens > 0 && total > 0 {
		unpriced := float64(result.Summary.EstimatedCost.UnpricedTokens) * 100 / float64(total)
		insight := analyticsInsight{
			ID:    "unpriced_share",
			Label: "Catalog coverage",
			Value: fmt.Sprintf("%.1f%%", unpriced),
			Detail: fmt.Sprintf("%s of %s window tokens lack catalog pricing and contribute no cost estimate.",
				commas(result.Summary.EstimatedCost.UnpricedTokens), commas(total)),
		}
		if tokedex {
			insight.LinkURL = "/tokedex#uncataloged-title"
			insight.LinkLabel = "Inspect in Tokedex"
		}
		insights = append(insights, insight)
	}
	return insights
}

func abs64(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

func unpricedSuffix(unpriced int64) string {
	if unpriced > 0 {
		return fmt.Sprintf(" · %s unpriced tokens", commas(unpriced))
	}
	return ""
}

// quotaView is one rendered row of the analytics page's quota panel.
// Percent is the measured usage against the primary budget dimension
// (tokens when a token budget exists, otherwise cost); Fill clamps it for
// the bar width so over-budget rows stay on the track.
type quotaView struct {
	database.QuotaStatus
	DisplayLabel string
	UsedLabel    string
	BudgetLabel  string
	Percent      float64
	Fill         float64
	Detail       string
	Over         bool
}

func quotaViews(statuses []database.QuotaStatus) []quotaView {
	views := make([]quotaView, 0, len(statuses))
	for _, status := range statuses {
		view := quotaView{QuotaStatus: status}
		view.DisplayLabel = quotaDisplayLabel(status)
		switch {
		case status.TokensBudget != nil:
			view.UsedLabel = commas(status.TokensUsed) + " tokens"
			view.BudgetLabel = commas(*status.TokensBudget) + " tokens"
			if *status.TokensBudget > 0 {
				view.Percent = float64(status.TokensUsed) * 100 / float64(*status.TokensBudget)
			}
		case status.CostBudget != nil:
			if status.CostUsed != nil {
				view.UsedLabel = money(*status.CostUsed)
				if *status.CostBudget > 0 {
					view.Percent = *status.CostUsed * 100 / *status.CostBudget
				}
			} else {
				view.UsedLabel = "Unknown"
			}
			view.BudgetLabel = money(*status.CostBudget)
		}
		view.Fill = view.Percent
		if view.Fill > 100 {
			view.Fill = 100
		}
		view.Over = status.State == "over"
		view.Detail = quotaDetail(status)
		views = append(views, view)
	}
	return views
}

func quotaDisplayLabel(status database.QuotaStatus) string {
	if status.Label != "" {
		return status.Label
	}
	switch {
	case status.Provider != "" && status.Model != "":
		return status.Provider + " · " + status.Model
	case status.Provider != "":
		return status.Provider
	default:
		return "All usage"
	}
}

// plural suffixes a count without pulling the dashboard package's helper.
func plural(count int64) string {
	if count == 1 {
		return ""
	}
	return "s"
}

// quotaDetail is the one-line explanation under each quota bar. Missing
// measurements say Unknown rather than presenting a zero.
func quotaDetail(status database.QuotaStatus) string {
	detail := ""
	switch {
	case status.State == "unknown":
		detail = "Cost unknown — no priced usage in this period yet."
	case status.TokensUsed == 0 && status.CostUsed == nil:
		detail = "No usage in this period yet."
	case status.ProjectedTokens != nil && status.ProjectedOverageDate != "":
		detail = fmt.Sprintf("Pace projects around %s tokens by %s; on this pace the budget is crossed around %s.",
			compact(*status.ProjectedTokens), status.PeriodEnd, status.ProjectedOverageDate)
	case status.ProjectedTokens != nil:
		detail = fmt.Sprintf("Pace projects around %s tokens by %s — within budget.",
			compact(*status.ProjectedTokens), status.PeriodEnd)
	case status.ProjectedCost != nil && status.ProjectedOverageDate != "":
		detail = fmt.Sprintf("Pace projects around %s by %s; on this pace the budget is crossed around %s.",
			money(*status.ProjectedCost), status.PeriodEnd, status.ProjectedOverageDate)
	case status.ProjectedCost != nil:
		detail = fmt.Sprintf("Pace projects around %s by %s — within budget.",
			money(*status.ProjectedCost), status.PeriodEnd)
	default:
		detail = fmt.Sprintf("Used since %s.", status.PeriodStart)
	}
	if status.UnknownTotalEvents > 0 {
		detail += fmt.Sprintf(" · %s event%s with unknown totals", commas(status.UnknownTotalEvents), plural(status.UnknownTotalEvents))
	}
	detail += fmt.Sprintf(" · resets %s", status.PeriodEnd)
	return detail
}

func analyticsChangeLabel(value *float64) string {
	if value == nil {
		return "No earlier baseline"
	}
	return fmt.Sprintf("%+.1f%%", *value)
}

func analyticsChangeClass(value *float64) string {
	if value == nil || *value == 0 {
		return "flat"
	}
	if *value > 0 {
		return "up"
	}
	return "down"
}

const maxMachineDetailProblems = 50

type machineAdapterGroup struct {
	Adapter          string
	State            string
	Summary          string
	ErrorSummary     string
	SingleAge        string
	SingleSourceHash string
	Count            int
	FreshCount       int
	StaleCount       int
	UnknownCount     int
	ErrorCount       int
}

type machineSourceSummary struct {
	Groups          []machineAdapterGroup
	ProblemSources  []database.MachineSourceHealth
	OmittedProblems int
	TotalSources    int
	ProblemCount    int
	HasDetails      bool
}

func groupMachineSources(sources []database.MachineSourceHealth) machineSourceSummary {
	if len(sources) == 0 {
		return machineSourceSummary{}
	}
	type adapterAcc struct {
		group  machineAdapterGroup
		errors map[string]struct{}
	}
	byAdapter := make(map[string]*adapterAcc)
	var allProblems []database.MachineSourceHealth
	for _, source := range sources {
		acc := byAdapter[source.Adapter]
		if acc == nil {
			acc = &adapterAcc{
				group:  machineAdapterGroup{Adapter: source.Adapter},
				errors: make(map[string]struct{}),
			}
			byAdapter[source.Adapter] = acc
		}
		acc.group.Count++
		switch source.State {
		case "Fresh":
			acc.group.FreshCount++
		case "Stale":
			acc.group.StaleCount++
		default:
			acc.group.UnknownCount++
		}
		if source.LastErrorClass != "" {
			acc.group.ErrorCount++
			acc.errors[source.LastErrorClass] = struct{}{}
		}
		if acc.group.Count == 1 {
			acc.group.SingleAge = source.Age
			acc.group.SingleSourceHash = source.SourceHash
		}
		if source.State != "Fresh" || source.LastErrorClass != "" {
			allProblems = append(allProblems, source)
		}
	}
	groups := make([]machineAdapterGroup, 0, len(byAdapter))
	for _, acc := range byAdapter {
		g := acc.group
		switch {
		case g.StaleCount > 0:
			g.State = "Stale"
		case g.UnknownCount > 0 && g.FreshCount == 0:
			g.State = "Unknown"
		case g.UnknownCount > 0:
			g.State = "Stale"
		default:
			g.State = "Fresh"
		}
		var parts []string
		if g.StaleCount > 0 {
			parts = append(parts, fmt.Sprintf("%d stale", g.StaleCount))
		}
		if g.UnknownCount > 0 {
			parts = append(parts, fmt.Sprintf("%d never synced", g.UnknownCount))
		}
		if g.FreshCount > 0 {
			parts = append(parts, fmt.Sprintf("%d fresh", g.FreshCount))
		}
		g.Summary = strings.Join(parts, " · ")
		if len(acc.errors) > 0 {
			errList := make([]string, 0, len(acc.errors))
			for errClass := range acc.errors {
				errList = append(errList, errClass)
			}
			sort.Strings(errList)
			g.ErrorSummary = strings.Join(errList, ", ")
		}
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool {
		li, lj := groups[i], groups[j]
		if (li.ErrorCount > 0) != (lj.ErrorCount > 0) {
			return li.ErrorCount > 0
		}
		if (li.State != "Fresh") != (lj.State != "Fresh") {
			return li.State != "Fresh"
		}
		return li.Adapter < lj.Adapter
	})
	sort.Slice(allProblems, func(i, j int) bool {
		pi, pj := allProblems[i], allProblems[j]
		if (pi.LastErrorClass != "") != (pj.LastErrorClass != "") {
			return pi.LastErrorClass != ""
		}
		if pi.State != pj.State {
			return sourceStateRank(pi.State) < sourceStateRank(pj.State)
		}
		if pi.Adapter != pj.Adapter {
			return pi.Adapter < pj.Adapter
		}
		return pi.SourceHash < pj.SourceHash
	})
	problemCount := len(allProblems)
	omitted := 0
	if len(allProblems) > maxMachineDetailProblems {
		omitted = len(allProblems) - maxMachineDetailProblems
		allProblems = allProblems[:maxMachineDetailProblems]
	}
	return machineSourceSummary{
		Groups:          groups,
		ProblemSources:  allProblems,
		OmittedProblems: omitted,
		TotalSources:    len(sources),
		ProblemCount:    problemCount,
		HasDetails:      len(sources) > len(groups),
	}
}

func sourceStateRank(state string) int {
	switch state {
	case "Stale":
		return 0
	case "Unknown":
		return 1
	default:
		return 2
	}
}

func conciseRelativeAge(parsed, now time.Time) string {
	elapsed := now.Sub(parsed)
	if elapsed < 0 {
		elapsed = 0
	}
	switch {
	case elapsed < time.Minute:
		return "just now"
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm ago", int(elapsed/time.Minute))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(elapsed/time.Hour))
	default:
		return fmt.Sprintf("%dd ago", int(elapsed/(24*time.Hour)))
	}
}

func analyticsShortTimeAt(raw string, loc *time.Location, now time.Time) template.HTML {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return template.HTML("—")
	}
	parsed, err := time.Parse(time.RFC3339Nano, trimmed)
	if err != nil {
		parsed, err = time.Parse(time.RFC3339, trimmed)
		if err != nil {
			return template.HTML(template.HTMLEscapeString(raw))
		}
	}
	if loc == nil {
		loc = time.UTC
	}
	local := parsed.In(loc)
	datetime := local.Format(time.RFC3339)
	title := local.Format("2006-01-02 15:04:05 MST")
	display := local.Format("Jan 2, 15:04") + " · " + conciseRelativeAge(parsed, now)
	return template.HTML(fmt.Sprintf(
		`<time datetime="%s" title="%s">%s</time>`,
		template.HTMLEscapeString(datetime),
		template.HTMLEscapeString(title),
		template.HTMLEscapeString(display),
	))
}

func parseQueryDate(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) != 10 {
		return ""
	}
	parsed, err := time.Parse("2006-01-02", raw)
	if err != nil || parsed.Format("2006-01-02") != raw {
		return ""
	}
	return raw
}

func validDateRange(query database.AnalyticsQuery) bool {
	if query.From == "" && query.To == "" {
		return true
	}
	if query.From != "" && parseQueryDate(query.From) == "" {
		return false
	}
	if query.To != "" && parseQueryDate(query.To) == "" {
		return false
	}
	if query.From != "" && query.To != "" && query.From > query.To {
		return false
	}
	return true
}

func analyticsQueryFromRequest(r *http.Request) database.AnalyticsQuery {
	values := r.URL.Query()
	period := values.Get("period")
	switch period {
	case "24h", "7d", "30d", "90d", "all":
	default:
		period = "30d"
	}
	dimension := values.Get("dimension")
	switch dimension {
	case "projects", "harnesses", "providers", "models", "machines":
	default:
		dimension = "projects"
	}
	return database.AnalyticsQuery{
		Period:    period,
		Dimension: dimension,
		Machine:   strings.TrimSpace(values.Get("machine")),
		Provider:  strings.TrimSpace(values.Get("provider")),
		Model:     strings.TrimSpace(values.Get("model")),
		Tool:      strings.TrimSpace(values.Get("tool")),
		Project:   strings.TrimSpace(values.Get("project")),
		Search:    strings.TrimSpace(values.Get("q")),
		From:      strings.TrimSpace(values.Get("from")),
		To:        strings.TrimSpace(values.Get("to")),
		Now:       time.Now().UTC(),
	}
}

func sessionPaginationFromRequest(r *http.Request) (page, pageSize int) {
	values := r.URL.Query()
	page = 1
	if raw := strings.TrimSpace(values.Get("page")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			page = parsed
		}
	}
	pageSize = database.DefaultSessionsPageSize
	rawSize := strings.TrimSpace(values.Get("page_size"))
	if rawSize == "" {
		rawSize = strings.TrimSpace(values.Get("limit"))
	}
	if rawSize != "" {
		if parsed, err := strconv.Atoi(rawSize); err == nil && parsed > 0 {
			if parsed > database.MaxSessionsPageSize {
				parsed = database.MaxSessionsPageSize
			}
			pageSize = parsed
		}
	}
	return page, pageSize
}

func analyticsQueryValues(query database.AnalyticsQuery) url.Values {
	values := url.Values{}
	values.Set("period", query.Period)
	values.Set("dimension", query.Dimension)
	if query.From != "" {
		values.Set("from", query.From)
	}
	if query.To != "" {
		values.Set("to", query.To)
	}
	if query.Machine != "" {
		values.Set("machine", query.Machine)
	}
	if query.Provider != "" {
		values.Set("provider", query.Provider)
	}
	if query.Model != "" {
		values.Set("model", query.Model)
	}
	if query.Tool != "" {
		values.Set("tool", query.Tool)
	}
	if query.Project != "" {
		values.Set("project", query.Project)
	}
	if query.Search != "" {
		values.Set("q", query.Search)
	}
	return values
}

func analyticsExportURL(query database.AnalyticsQuery, format string) string {
	values := analyticsQueryValues(query)
	if format != "" {
		values.Set("format", format)
	}
	return "/api/v1/analytics/export?" + values.Encode()
}

func sessionsFilterValues(query database.AnalyticsQuery) url.Values {
	values := url.Values{}
	if query.Period != "" {
		values.Set("period", query.Period)
	}
	if query.From != "" {
		values.Set("from", query.From)
	}
	if query.To != "" {
		values.Set("to", query.To)
	}
	if query.Machine != "" {
		values.Set("machine", query.Machine)
	}
	if query.Provider != "" {
		values.Set("provider", query.Provider)
	}
	if query.Model != "" {
		values.Set("model", query.Model)
	}
	if query.Tool != "" {
		values.Set("tool", query.Tool)
	}
	if query.Project != "" {
		values.Set("project", query.Project)
	}
	if query.Search != "" {
		values.Set("q", query.Search)
	}
	return values
}

func sessionsExportURL(query database.AnalyticsQuery, format string) string {
	values := sessionsFilterValues(query)
	if format != "" {
		values.Set("format", format)
	}
	return "/api/v1/sessions/export?" + values.Encode()
}

func sessionsPageURL(query database.AnalyticsQuery, sort, dir string, page, pageSize int) string {
	values := sessionsFilterValues(query)
	if sort != "" && (sort != "when" || dir != "desc") {
		values.Set("sort", sort)
		values.Set("dir", dir)
	}
	if page > 1 {
		values.Set("page", strconv.Itoa(page))
	}
	if pageSize > 0 && pageSize != database.DefaultSessionsPageSize {
		values.Set("page_size", strconv.Itoa(pageSize))
	}
	encoded := values.Encode()
	if encoded == "" {
		return "/sessions"
	}
	return "/sessions?" + encoded
}

func sessionsActiveFilters(query database.AnalyticsQuery, sort, dir string, pageSize int, modelAliases, machineAliases map[string]string) []activeFilterPill {
	var pills []activeFilterPill
	addPill := func(label string, mutate func(*database.AnalyticsQuery)) {
		next := query
		mutate(&next)
		pills = append(pills, activeFilterPill{
			Label:     label,
			RemoveURL: sessionsPageURL(next, sort, dir, 1, pageSize),
		})
	}
	if query.Search != "" {
		addPill("Search: "+query.Search, func(q *database.AnalyticsQuery) { q.Search = "" })
	}
	if query.From != "" {
		addPill("From: "+query.From, func(q *database.AnalyticsQuery) { q.From = "" })
	}
	if query.To != "" {
		addPill("To: "+query.To, func(q *database.AnalyticsQuery) { q.To = "" })
	}
	if query.Project != "" {
		label := query.Project
		if label == database.UnknownProjectFilter {
			label = "Unknown project"
		}
		addPill("Project: "+label, func(q *database.AnalyticsQuery) { q.Project = "" })
	}
	if query.Machine != "" {
		addPill("Machine: "+machineDisplayName(machineAliases, query.Machine), func(q *database.AnalyticsQuery) { q.Machine = "" })
	}
	if query.Provider != "" {
		addPill("Provider: "+query.Provider, func(q *database.AnalyticsQuery) { q.Provider = "" })
	}
	if query.Model != "" {
		addPill("Model: "+modelDisplayName(modelAliases, query.Model), func(q *database.AnalyticsQuery) { q.Model = "" })
	}
	if query.Tool != "" {
		addPill("Harness: "+harnessName(query.Tool), func(q *database.AnalyticsQuery) { q.Tool = "" })
	}
	if query.From == "" && query.To == "" && query.Period != "" && query.Period != "30d" {
		addPill("Period: "+strings.ToUpper(query.Period), func(q *database.AnalyticsQuery) { q.Period = "30d" })
	}
	return pills
}

func sessionDetailURL(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "/sessions"
	}
	return "/sessions/" + url.PathEscape(id)
}

func tokedexDetailURL(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "/tokedex"
	}
	parts := strings.Split(id, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return "/tokedex/" + strings.Join(parts, "/")
}

func isDrillableBucket(name string) bool {
	return strings.TrimSpace(name) != "" && !isUnknownBucketName(name)
}

func overviewDrillURL(kind, value string) string {
	if !isDrillableBucket(value) {
		return ""
	}
	switch kind {
	case "project":
		return "/sessions?period=all&project=" + url.QueryEscape(value)
	case "tool":
		return "/analytics?period=all&dimension=projects&tool=" + url.QueryEscape(value)
	case "model":
		return "/analytics?period=all&dimension=projects&model=" + url.QueryEscape(value)
	case "machine":
		return "/analytics?period=all&dimension=projects&machine=" + url.QueryEscape(value)
	default:
		return ""
	}
}

func analyticsBreakdownDrillURL(query database.AnalyticsQuery, name string) string {
	if !isDrillableBucket(name) {
		return ""
	}
	if query.Dimension == "projects" {
		next := query
		next.Project = name
		encoded := sessionsFilterValues(next).Encode()
		if encoded == "" {
			return "/sessions"
		}
		return "/sessions?" + encoded
	}
	next := query
	switch query.Dimension {
	case "machines":
		next.Machine = name
	case "providers":
		next.Provider = name
	case "models":
		next.Model = name
	case "harnesses":
		next.Tool = name
	default:
		return ""
	}
	nextDim := "projects"
	if next.Project != "" {
		if query.Dimension == "models" {
			nextDim = "harnesses"
		} else {
			nextDim = "models"
		}
	}
	return analyticsViewURL(next, nextDim)
}

func analyticsViewURL(query database.AnalyticsQuery, dimension string) string {
	query.Dimension = dimension
	return "/analytics?" + analyticsQueryValues(query).Encode()
}

func analyticsPeriodURL(query database.AnalyticsQuery, period string) string {
	query.Period = period
	query.From = ""
	query.To = ""
	return analyticsViewURL(query, query.Dimension)
}

func analyticsActiveFilters(query database.AnalyticsQuery, modelAliases, machineAliases map[string]string) []activeFilterPill {
	var pills []activeFilterPill
	addPill := func(label string, mutate func(*database.AnalyticsQuery)) {
		next := query
		mutate(&next)
		pills = append(pills, activeFilterPill{
			Label:     label,
			RemoveURL: analyticsViewURL(next, query.Dimension),
		})
	}
	if query.From != "" {
		addPill("From: "+query.From, func(q *database.AnalyticsQuery) { q.From = "" })
	}
	if query.To != "" {
		addPill("To: "+query.To, func(q *database.AnalyticsQuery) { q.To = "" })
	}
	if query.Project != "" {
		label := query.Project
		if label == database.UnknownProjectFilter {
			label = "Unknown project"
		}
		addPill("Project: "+label, func(q *database.AnalyticsQuery) { q.Project = "" })
	}
	if query.Machine != "" {
		addPill("Machine: "+machineDisplayName(machineAliases, query.Machine), func(q *database.AnalyticsQuery) { q.Machine = "" })
	}
	if query.Provider != "" {
		addPill("Provider: "+query.Provider, func(q *database.AnalyticsQuery) { q.Provider = "" })
	}
	if query.Model != "" {
		addPill("Model: "+modelDisplayName(modelAliases, query.Model), func(q *database.AnalyticsQuery) { q.Model = "" })
	}
	if query.Tool != "" {
		addPill("Harness: "+harnessName(query.Tool), func(q *database.AnalyticsQuery) { q.Tool = "" })
	}
	return pills
}

func analyticsClearFiltersURL(query database.AnalyticsQuery) string {
	return analyticsViewURL(database.AnalyticsQuery{Period: query.Period, Dimension: query.Dimension}, query.Dimension)
}

func analyticsDateRangeLabel(filter database.AnalyticsQuery, startDate, endDate string) string {
	if !validDateRange(filter) || startDate == "" || endDate == "" {
		return "Invalid date range"
	}
	if startDate == endDate {
		return startDate
	}
	return startDate + " → " + endDate
}

func dashboardETag(html []byte) string {
	if len(html) == 0 {
		return ""
	}
	sum := sha256.Sum256(html)
	return fmt.Sprintf(`"%x"`, sum[:16])
}

func etagMatches(ifNoneMatch, etag string) bool {
	if etag == "" || strings.TrimSpace(ifNoneMatch) == "" {
		return false
	}
	normalizeTag := func(tag string) string {
		tag = strings.TrimSpace(tag)
		if strings.HasPrefix(tag, "W/") || strings.HasPrefix(tag, "w/") {
			tag = strings.TrimSpace(tag[2:])
		}
		return tag
	}
	target := normalizeTag(etag)
	for _, candidate := range strings.Split(ifNoneMatch, ",") {
		part := strings.TrimSpace(candidate)
		if part == "*" {
			return true
		}
		if normalizeTag(part) == target {
			return true
		}
	}
	return false
}

func insightsViewURL(query database.AnalyticsQuery, dimension string) string {
	query.Dimension = dimension
	return "/insights?" + analyticsQueryValues(query).Encode()
}

func insightsPeriodURL(query database.AnalyticsQuery, period string) string {
	query.Period = period
	return insightsViewURL(query, query.Dimension)
}

func insightBarPercent(value, maximum int64) float64 {
	if value <= 0 || maximum <= 0 {
		return 0
	}
	percent := float64(value) * 100 / float64(maximum)
	if percent < 2 {
		return 2
	}
	if percent > 100 {
		return 100
	}
	return percent
}

func insightDistributionMax(values []database.InsightDistribution) int64 {
	var maximum int64
	for _, value := range values {
		if value.Tokens > maximum {
			maximum = value.Tokens
		}
	}
	return maximum
}

func insightCoverage(value float64) string {
	if value <= 0 {
		return "0%"
	}
	if value >= 1 {
		return "100%"
	}
	return fmt.Sprintf("%.0f%%", value*100)
}

func insightRecapLabel(status InsightsRecapStatus) string {
	switch status {
	case InsightsRecapGenerated:
		return "Generated recap"
	case InsightsRecapUnavailable:
		return "Recap temporarily unavailable"
	default:
		return "Recap disabled / unconfigured"
	}
}

func aliasMaps(values []database.DisplayAlias) (map[string]string, map[string]string) {
	models := make(map[string]string)
	machines := make(map[string]string)
	for _, value := range values {
		switch value.Kind {
		case database.AliasKindModel:
			models[value.Identity] = value.Alias
		case database.AliasKindMachine:
			machines[value.Identity] = value.Alias
		}
	}
	return models, machines
}

func compareAliasRows(left, right aliasRow) bool {
	if left.HasUsage != right.HasUsage {
		return left.HasUsage
	}
	if left.Tokens != right.Tokens {
		return left.Tokens > right.Tokens
	}
	if left.Events != right.Events {
		return left.Events > right.Events
	}
	return left.Identity < right.Identity
}

func modelAliasRows(values []database.ModelTotal, aliases map[string]string, tokedex bool) []aliasRow {
	rows := make([]aliasRow, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		seen[value.Model] = struct{}{}
		var analyticsURL, tokedexURL string
		if isDrillableBucket(value.Model) {
			analyticsURL = "/analytics?dimension=models&model=" + url.QueryEscape(value.Model)
			if tokedex {
				tokedexURL = "/tokedex?q=" + url.QueryEscape(value.Model)
			}
		}
		rows = append(rows, aliasRow{
			Identity:      value.Model,
			Alias:         aliases[value.Model],
			Default:       compactModelName(value.Model),
			Tokens:        value.Tokens,
			Events:        value.Events,
			UnknownEvents: value.UnknownEvents,
			HasUsage:      value.Events > 0,
			AnalyticsURL:  analyticsURL,
			TokedexURL:    tokedexURL,
		})
	}
	for identity, alias := range aliases {
		if _, ok := seen[identity]; !ok {
			var analyticsURL, tokedexURL string
			if isDrillableBucket(identity) {
				analyticsURL = "/analytics?dimension=models&model=" + url.QueryEscape(identity)
				if tokedex {
					tokedexURL = "/tokedex?q=" + url.QueryEscape(identity)
				}
			}
			rows = append(rows, aliasRow{
				Identity:     identity,
				Alias:        alias,
				Default:      compactModelName(identity),
				AnalyticsURL: analyticsURL,
				TokedexURL:   tokedexURL,
			})
		}
	}
	sort.SliceStable(rows, func(left, right int) bool { return compareAliasRows(rows[left], rows[right]) })
	return rows
}

func machineAliasRows(values []database.MachineTotal, aliases map[string]string) []aliasRow {
	rows := make([]aliasRow, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		seen[value.Machine] = struct{}{}
		var analyticsURL string
		if isDrillableBucket(value.Machine) {
			analyticsURL = "/analytics?dimension=machines&machine=" + url.QueryEscape(value.Machine)
		}
		rows = append(rows, aliasRow{
			Identity:      value.Machine,
			Alias:         aliases[value.Machine],
			Default:       compactMachineName(value.Machine),
			Tokens:        value.Tokens,
			Events:        value.Events,
			UnknownEvents: value.UnknownEvents,
			HasUsage:      value.Events > 0,
			AnalyticsURL:  analyticsURL,
		})
	}
	for identity, alias := range aliases {
		if _, ok := seen[identity]; !ok {
			var analyticsURL string
			if isDrillableBucket(identity) {
				analyticsURL = "/analytics?dimension=machines&machine=" + url.QueryEscape(identity)
			}
			rows = append(rows, aliasRow{
				Identity:     identity,
				Alias:        alias,
				Default:      compactMachineName(identity),
				AnalyticsURL: analyticsURL,
			})
		}
	}
	sort.SliceStable(rows, func(left, right int) bool { return compareAliasRows(rows[left], rows[right]) })
	return rows
}

func limitDashboardRows[T any](values []T) []T {
	if len(values) <= dashboardUsageRowsLimit {
		return values
	}
	return values[:dashboardUsageRowsLimit]
}

func New(store *database.Store, ingestToken string) (*Server, error) {
	return NewWithConfig(store, Config{IngestToken: ingestToken, DashboardToken: ingestToken})
}

// NewWithInsightsAI enables optional aggregate-only AI synthesis. The caller
// should use New when synthesis is not explicitly configured so the page can
// distinguish disabled from temporarily unavailable.
func NewWithInsightsAI(store *database.Store, ingestToken string, synthesizer insightai.Synthesizer) (*Server, error) {
	if synthesizer == nil {
		return New(store, ingestToken)
	}
	return NewWithInsights(store, ingestToken, aggregateAIRecapProvider{synthesizer: synthesizer})
}

// NewWithConfigAndInsightsAI builds a server from an explicit security and
// lifecycle configuration plus an optional aggregate-only AI synthesizer.
func NewWithConfigAndInsightsAI(store *database.Store, config Config, synthesizer insightai.Synthesizer) (*Server, error) {
	if synthesizer == nil {
		return NewWithConfig(store, config)
	}
	return NewWithConfig(store, config, aggregateAIRecapProvider{synthesizer: synthesizer})
}

// NewWithInsights keeps the original New constructor source-compatible while
// allowing an aggregate-only recap provider to be injected.
func NewWithInsights(store *database.Store, ingestToken string, providers ...InsightsRecapProvider) (*Server, error) {
	return NewWithConfig(store, Config{IngestToken: ingestToken, DashboardToken: ingestToken}, providers...)
}

func NewWithConfig(store *database.Store, config Config, providers ...InsightsRecapProvider) (*Server, error) {
	var recap InsightsRecapProvider
	if len(providers) > 0 {
		recap = providers[0]
	}
	config = config.normalized()
	if err := config.validateDashboardPasswordAuth(); err != nil {
		return nil, err
	}
	if err := config.validateAlertConfig(); err != nil {
		return nil, err
	}
	trustedProxyNetworks, err := parseTrustedProxyCIDRs(config.TrustedProxyCIDRs)
	if err != nil {
		return nil, err
	}
	csrfSecret, err := newCSRFSecret(config.DashboardToken)
	if err != nil {
		return nil, err
	}
	sessionSecret, err := newSessionSecret(config.DashboardToken)
	if err != nil {
		return nil, err
	}
	staticFiles, err := fs.Sub(web.StaticFS, "static")
	if err != nil {
		return nil, err
	}
	assetVersion := embeddedStaticVersion()
	artDims := loadArtDimensions(staticFiles)
	page, err := template.New("").Funcs(template.FuncMap{
		"tokedexEnabled":     func() bool { return config.EnableTokedex },
		"commas":             commas,
		"commasPtr":          commasPtr,
		"activityTooltip":    activityTooltip,
		"activityInitialTab": activityInitialTab,
		"activityTabStop":    activityTabStop,
		"mul":                func(left, right float64) float64 { return left * right },
		"percent":            func(value float64) float64 { return value * 100 },
		"money":              money,
		"compact":            compact,
		"cacheUncached":      cacheUncached,
		"compactPtr":         compactPtr,
		"share": func(part, total int64) string {
			if total == 0 {
				return "0.0%"
			}
			return fmt.Sprintf("%.1f%%", float64(part)*100/float64(total))
		},
		"topProjects":         topProjects,
		"topModels":           topModels,
		"topTools":            topTools,
		"harnessName":         harnessName,
		"modelDisplayName":    modelDisplayName,
		"machineDisplayName":  machineDisplayName,
		"analyticsBar":        analyticsBarPercent,
		"analyticsPart":       analyticsPartPercent,
		"analyticsDateTick":   analyticsDateTickClass,
		"analyticsDateLabel":  analyticsDateTickLabel,
		"shareDateLabel":      analyticsShareDateTickLabel,
		"analyticsTickPeriod": analyticsTickPeriod,
		"dimensionLabel":      analyticsDimensionLabel,
		"unknownLabel":        analyticsUnknownLabel,
		"isUnknownName":       isUnknownBucketName,
		"breakdownName":       breakdownDisplayName,
		"analyticsTime": func(raw string) template.HTML {
			return analyticsShortTimeAt(raw, store.Location(), time.Now())
		},
		"machineSourceGroups": groupMachineSources,
		"compositionTotal": func(comp database.TokenComposition) int64 {
			return comp.InputTokens + comp.OutputTokens
		},
		"compositionWidth": func(part, total int64) float64 {
			if part <= 0 || total <= 0 {
				return 0
			}
			return float64(part) * 100 / float64(total)
		},
		"compositionShare": func(part, total int64) string {
			if total <= 0 {
				return "—"
			}
			return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(part)*100/float64(total)), ".0") + "%"
		},
		"compositionTooltip": func(label string, value int64) string {
			return label + "\n" + commas(value) + " tokens"
		},
		"analyticsURL":             analyticsViewURL,
		"analyticsPeriodURL":       analyticsPeriodURL,
		"analyticsClearFiltersURL": analyticsClearFiltersURL,
		"analyticsDateRangeLabel":  analyticsDateRangeLabel,
		"sessionsExportURL":        sessionsExportURL,
		"sessionsPageURL":          sessionsPageURL,
		"sessionDetailURL":         sessionDetailURL,
		"tokedexDetailURL":         tokedexDetailURL,
		"validDateInput":           parseQueryDate,
		"validDateRange":           validDateRange,
		"overviewDrillURL":         overviewDrillURL,
		"breakdownDrillURL":        analyticsBreakdownDrillURL,
		"unknownProjectFilter":     func() string { return database.UnknownProjectFilter },
		"trendPointDate": func(point database.AnalyticsPoint, bucket string) string {
			date, _, _ := analyticsPointDateRange(point, bucket, store.Location())
			return date
		},
		"trendPointFrom": func(point database.AnalyticsPoint, bucket string) string {
			_, from, _ := analyticsPointDateRange(point, bucket, store.Location())
			return from
		},
		"trendPointTo": func(point database.AnalyticsPoint, bucket string) string {
			_, _, to := analyticsPointDateRange(point, bucket, store.Location())
			return to
		},
		"analyticsChange":      analyticsChangeLabel,
		"analyticsChangeClass": analyticsChangeClass,
		"insightsURL":          insightsViewURL,
		"insightsPeriodURL":    insightsPeriodURL,
		"insightBar":           insightBarPercent,
		"insightCoverage":      insightCoverage,
		"insightRecapLabel":    insightRecapLabel,
		"sessionDisplayName":   sessionDisplayName,
		"copyID":               copyIDButton,
		"copyIDScript":         func() template.HTML { return template.HTML(copyIDScript) },
		"sortableHeader":       sortableHeader,
		"compactTokenValue": func(value *int64) string {
			if value == nil {
				return "Unknown"
			}
			return compact(*value)
		},
		"ptrInt64": func(v int64) *int64 { return &v },
		"hasString": func(list []string, target string) bool {
			return slices.Contains(list, target)
		},
		"tokenValue": func(value *int64) string {
			if value == nil {
				return "Unknown"
			}
			return commas(*value)
		},
		"costValue": func(value *float64, estimated bool) string {
			if value == nil {
				return "Unknown"
			}
			// Sub-dollar session costs keep enough precision to stay nonzero.
			label := fmt.Sprintf("$%.4f", *value)
			if *value >= 1 {
				label = money(*value)
			}
			if estimated {
				return label + " estimated"
			}
			return label
		},
		"durationValue": func(value *int64) string {
			if value == nil {
				return "Unknown"
			}
			return durationLabel(*value)
		},
		"priceValue": func(value *float64) string {
			if value == nil {
				// Unknown is a value, not zero: a missing rate must never
				// render as a free model or as an empty cell.
				return "Unknown"
			}
			return fmt.Sprintf("$%.3f/M", *value)
		},
		"formatDetailCost": formatSubDollarCost,
		"formatRatePtr": func(value *float64) string {
			if value == nil {
				return "Unknown"
			}
			return money(*value) + "/1M"
		},
		"joinStrings": func(values []string) string {
			return strings.Join(values, ", ")
		},
		// pricingKnown reports whether a catalog entry carries the published
		// rates the dashboard can honestly price usage with. It mirrors the
		// PricedTokens accounting in Store.CatalogSnapshot: standard mode plus
		// both input and output rates.
		"pricingKnown": func(pricing catalog.Pricing) bool {
			return pricing.Mode == "standard" && pricing.InputPricePerMillion != nil && pricing.OutputPricePerMillion != nil
		},
		"statusClass": func(status string) string {
			return strings.ToLower(status)
		},
		// sourceStateClass maps a per-source freshness state onto the shared
		// status-pill classes: Fresh is connected-green, Stale warm, Unknown
		// the neutral pill (same mapping as the data freshness pill).
		"sourceStateClass": func(state string) string {
			switch state {
			case "Fresh":
				return "connected"
			case "Stale":
				return "stale"
			default:
				return ""
			}
		},
		"joinAdapters": func(values []string) string {
			return strings.Join(values, " · ")
		},
		"glyphCell":    glyphCellClass,
		"projectGlyph": glyphForProject,
		"harnessGlyph": glyphForHarness,
		"modelGlyph":   glyphForModel,
		"machineGlyph": glyphForMachine,
		"staticURL":    func(path string) string { return staticAssetURL(path, assetVersion) },
		"assetPath": func(stage int) string {
			return staticAssetURL("/static/tokemon/stage-"+twoDigits(stage)+".png", assetVersion)
		},
		"assetWidth": func(stage int) int {
			if dims, ok := artDims[stage]; ok && dims[0] > 0 {
				return dims[0]
			}
			return 640
		},
		"assetHeight": func(stage int) int {
			if dims, ok := artDims[stage]; ok && dims[1] > 0 {
				return dims[1]
			}
			return 640
		},
	}).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	bgCtx, bgCancel := context.WithCancel(context.Background())
	return &Server{
		store:                store,
		config:               config,
		logger:               config.Logger,
		metrics:              config.Metrics,
		csrfSecret:           csrfSecret,
		sessionSecret:        sessionSecret,
		trustedProxyNetworks: trustedProxyNetworks,
		loginThrottle:        newLoginThrottle(),
		template:             page,
		static:               http.FileServer(http.FS(staticFiles)),
		assetVersion:         assetVersion,
		recapProvider:        recap,
		alerts:               newQuotaAlertState(config.AlertHTTPClient),
		bgCtx:                bgCtx,
		bgCancel:             bgCancel,
	}, nil
}

func (s *Server) backgroundContext() context.Context {
	if s.bgCtx != nil {
		return s.bgCtx
	}
	return context.Background()
}

func (s *Server) tryStartBackground() bool {
	s.bgMu.Lock()
	defer s.bgMu.Unlock()
	if s.bgClosed || s.bgCtx == nil {
		return false
	}
	s.bgWG.Add(1)
	return true
}

// Close cancels any in-flight background cache refreshes and waits for them to
// finish so callers can safely close the underlying Store afterwards.
func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.bgMu.Lock()
		s.bgClosed = true
		cancel := s.bgCancel
		s.bgMu.Unlock()
		if cancel != nil {
			cancel()
		}
		s.bgWG.Wait()
	})
	return s.closeErr
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", s.withDeadline(s.config.HealthTimeout, http.HandlerFunc(s.health)))
	mux.Handle("GET /static/", staticCacheHeaders(http.StripPrefix("/static/", s.static)))
	dashboard := func(handler http.Handler) http.Handler {
		return s.withDeadline(s.config.RequestTimeout, s.dashboardAuth(handler))
	}
	mux.Handle("GET /login", s.withDeadline(s.config.RequestTimeout, http.HandlerFunc(s.loginPage)))
	mux.Handle("POST /login", s.withDeadline(s.config.RequestTimeout, http.HandlerFunc(s.loginSubmit)))
	mux.Handle("POST /logout", s.withDeadline(s.config.RequestTimeout, http.HandlerFunc(s.logout)))
	mux.Handle("GET /", dashboard(http.HandlerFunc(s.dashboard)))
	mux.Handle("GET /analytics", dashboard(http.HandlerFunc(s.analyticsPage)))
	mux.Handle("GET /data", dashboard(http.HandlerFunc(s.analyticsPage)))
	mux.Handle("GET /insights", dashboard(http.HandlerFunc(s.insightsPage)))
	mux.Handle("GET /api/v1/insights", dashboard(http.HandlerFunc(s.insightsAPI)))
	mux.Handle("GET /tokedex", dashboard(http.HandlerFunc(s.catalogPage)))
	mux.Handle("GET /tokedex/{model...}", dashboard(http.HandlerFunc(s.tokedexDetailPage)))
	mux.Handle("GET /catalog", dashboard(http.HandlerFunc(s.catalogPage)))
	mux.Handle("GET /sessions", dashboard(http.HandlerFunc(s.sessionsPage)))
	mux.Handle("GET /sessions/", dashboard(http.NotFoundHandler()))
	mux.Handle("GET /sessions/{id}", dashboard(http.HandlerFunc(s.sessionDetailPage)))
	mux.Handle("GET /settings", dashboard(http.HandlerFunc(s.settings)))
	mux.Handle("GET /settings/aliases", dashboard(http.HandlerFunc(s.settings)))
	mux.Handle("POST /settings/aliases", dashboard(http.HandlerFunc(s.saveSettings)))
	mux.Handle("POST /settings/quotas", dashboard(http.HandlerFunc(s.saveQuota)))
	mux.Handle("POST /api/v1/events/batch", s.withDeadline(s.config.IngestTimeout, http.HandlerFunc(s.ingest)))
	mux.Handle("POST /api/v1/agents/heartbeat", s.withDeadline(s.config.HeartbeatTimeout, http.HandlerFunc(s.heartbeat)))
	mux.Handle("GET /api/v1/machines", dashboard(http.HandlerFunc(s.machines)))
	mux.Handle("GET /api/v1/quotas", dashboard(http.HandlerFunc(s.quotasAPI)))
	mux.Handle("GET /api/v1/catalog", dashboard(http.HandlerFunc(s.catalogAPI)))
	mux.Handle("GET /api/v1/models", dashboard(http.HandlerFunc(s.modelsAPI)))
	mux.Handle("GET /api/v1/sessions", dashboard(http.HandlerFunc(s.sessionsAPI)))
	mux.Handle("GET /api/v1/sessions/export", dashboard(http.HandlerFunc(s.sessionsExport)))
	mux.Handle("GET /api/v1/evolution", dashboard(http.HandlerFunc(s.evolution)))
	mux.Handle("GET /api/v1/analytics/overview", dashboard(http.HandlerFunc(s.overview)))
	mux.Handle("GET /api/v1/analytics", dashboard(http.HandlerFunc(s.analyticsAPI)))
	mux.Handle("GET /api/v1/analytics/timeline", dashboard(http.HandlerFunc(s.timelineAPI)))
	mux.Handle("GET /api/v1/analytics/export", dashboard(http.HandlerFunc(s.analyticsExport)))
	mux.Handle("GET /api/v1/card.svg", dashboard(http.HandlerFunc(s.shareCardSVG)))
	mux.Handle("GET /metrics", dashboard(http.HandlerFunc(s.metricsHandler)))
	return s.requestLogging(securityHeaders(gzipMiddleware(mux)))
}

func (s *Server) withDeadline(timeout time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if timeout <= 0 {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type responseStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseStatusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseStatusWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

// Unwrap preserves access to optional ResponseWriter capabilities through
// middleware (for example, http.NewResponseController). The server currently
// renders bounded responses, but retaining the underlying writer avoids
// surprising handlers when the API grows streaming endpoints.
func (w *responseStatusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) requestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		response := &responseStatusWriter{ResponseWriter: w}
		s.metrics.requestsTotal.Add(1)
		next.ServeHTTP(response, r)
		status := response.status
		if status == 0 {
			status = http.StatusOK
		}
		if status >= http.StatusInternalServerError {
			s.metrics.requestFailures.Add(1)
		}
		s.logger.Info("http_request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"duration_ms", time.Since(started).Milliseconds(),
		)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// same-origin (not no-referrer): the login and logout forms must send
		// a same-origin Referer/Origin for the POST origin check, while
		// cross-origin requests still carry no referrer at all.
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	err := s.store.Ready(r.Context())
	s.metrics.observeDB(started)
	if err != nil {
		s.logger.Warn("readiness_failed", "status", http.StatusServiceUnavailable)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	stale, err := s.store.StaleAgents(r.Context(), time.Now().Add(-s.config.StaleAgentAfter))
	s.metrics.observeDB(started)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "metrics unavailable"})
		return
	}
	snapshot := s.metrics.snapshot()
	snapshot.StaleAgents = stale
	writeJSON(w, http.StatusOK, snapshot)
}

type batchRequest struct {
	Events []usage.Event `json:"events"`
}

type heartbeatAdapter struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// heartbeatSource is one source's connector health. A nil Sources slice on
// the request means an older agent without the field: the hub keeps that
// machine's previously reported rows. An empty non-nil slice is a health-
// aware agent with zero discovered sources and clears the rows.
type heartbeatSource struct {
	Adapter            string     `json:"adapter"`
	IdentityHash       string     `json:"identity_hash"`
	LastSuccessfulSync *time.Time `json:"last_successful_sync"`
	LastErrorClass     string     `json:"last_error_class"`
}

type heartbeatRequest struct {
	MachineID        string             `json:"machine_id"`
	AgentVersion     string             `json:"agent_version"`
	OperatingSystem  string             `json:"operating_system"`
	Architecture     string             `json:"architecture"`
	Adapters         []heartbeatAdapter `json:"adapters"`
	SourceCount      int                `json:"source_count"`
	SourceErrorCount int                `json:"source_error_count"`
	Sources          []heartbeatSource  `json:"sources"`
}

// maxHeartbeatBodyBytes bounds the decompressed heartbeat payload; ingest
// batch limits live beside the shared body decoder in security.go.
const maxHeartbeatBodyBytes = 1 << 20

// maxHeartbeatSources bounds the per-source health list. A machine's source
// list is one row per transcript/database, so it grows with session history;
// the old 256 cap rejected busy machines outright (macOS with 297 sources),
// starving the machine view and its connector health.
const maxHeartbeatSources = 4096

func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	s.metrics.ingestRequests.Add(1)
	if !s.ingestAuthorized(r) {
		s.rejectIngest(w, http.StatusUnauthorized, "invalid ingest token", 1)
		return
	}
	var request batchRequest
	if err := decodeJSONBody(w, r, maxDecompressedBodyBytes, &request); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errBodyTooLarge) || errors.Is(err, errBodyRead) {
			status = http.StatusRequestEntityTooLarge
		}
		s.rejectIngest(w, status, "invalid or oversized event batch", 1)
		return
	}
	if len(request.Events) == 0 || len(request.Events) > maxBatchEvents {
		s.rejectIngest(w, http.StatusBadRequest, "event batch count is outside the permitted range", len(request.Events))
		return
	}
	encodedBatch, err := json.Marshal(request)
	if err != nil || int64(len(encodedBatch)) > maxBatchBodyBytes {
		s.rejectIngest(w, http.StatusRequestEntityTooLarge, "event batch is too large", len(request.Events))
		return
	}
	for index, event := range request.Events {
		if err := event.Validate(); err != nil {
			// Do not echo validation details or payload fields to a remote caller.
			s.rejectIngest(w, http.StatusBadRequest, fmt.Sprintf("event %d is invalid", index+1), len(request.Events))
			return
		}
	}
	started := time.Now()
	result, err := s.store.Ingest(r.Context(), request.Events)
	s.metrics.observeDB(started)
	if err != nil {
		s.logger.Error("ingest_failed", "events", len(request.Events))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not persist event batch"})
		return
	}
	s.metrics.ingestAccepted.Add(uint64(result.Accepted))
	s.metrics.ingestDuplicates.Add(uint64(result.Duplicates))
	s.metrics.ingestRejected.Add(uint64(result.Rejected))
	s.logger.Info("ingest_completed", "events", len(request.Events), "accepted", result.Accepted, "duplicates", result.Duplicates, "rejected", result.Rejected)
	s.invalidateEvolutionCache()
	s.invalidateOverviewCache()
	s.invalidateAnalyticsCache()
	s.invalidateCatalogCache()
	s.invalidateSessionsCache()
	s.invalidateInsightsCache()
	if result.Accepted > 0 {
		s.triggerQuotaAlerts(true)
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) rejectIngest(w http.ResponseWriter, status int, message string, count int) {
	if count < 1 {
		count = 1
	}
	s.metrics.ingestRejected.Add(uint64(count))
	s.logger.Warn("ingest_rejected", "rejected", count)
	writeJSON(w, status, map[string]string{"error": message})
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	if !s.ingestAuthorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid ingest token"})
		return
	}
	var request heartbeatRequest
	if err := decodeJSONBody(w, r, maxHeartbeatBodyBytes, &request); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errBodyTooLarge) || errors.Is(err, errBodyRead) {
			status = http.StatusRequestEntityTooLarge
		}
		writeJSON(w, status, map[string]string{"error": "invalid or oversized heartbeat"})
		return
	}
	if strings.TrimSpace(request.MachineID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "machine_id is required"})
		return
	}
	if len(request.MachineID) > 128 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "machine_id is too long"})
		return
	}
	if len(request.AgentVersion) > 64 || len(request.OperatingSystem) > 32 || len(request.Architecture) > 16 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "heartbeat metadata field is too long"})
		return
	}
	if request.SourceCount < 0 || request.SourceErrorCount < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source counts cannot be negative"})
		return
	}
	if len(request.Adapters) > 64 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "too many adapters"})
		return
	}
	if len(request.Sources) > maxHeartbeatSources {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "too many sources"})
		return
	}
	syncClockSkewLimit := 24 * time.Hour
	for _, source := range request.Sources {
		if strings.TrimSpace(source.Adapter) == "" || len([]rune(source.Adapter)) > 128 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source adapters must be non-empty and at most 128 characters"})
			return
		}
		if strings.TrimSpace(source.IdentityHash) == "" || len([]rune(source.IdentityHash)) > 128 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source identity hashes must be non-empty and at most 128 characters"})
			return
		}
		if len(source.LastErrorClass) > 32 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source error class is too long"})
			return
		}
		if source.LastSuccessfulSync != nil && time.Until(source.LastSuccessfulSync.UTC()) > syncClockSkewLimit {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source sync timestamp is too far in the future"})
			return
		}
	}
	adapterIDs := make([]string, 0, len(request.Adapters))
	for _, adapter := range request.Adapters {
		if strings.TrimSpace(adapter.ID) == "" || len([]rune(adapter.ID)) > 128 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "adapter IDs must be non-empty and at most 128 characters"})
			return
		}
		if len(adapter.Version) > 64 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "adapter version is too long"})
			return
		}
		adapterIDs = append(adapterIDs, adapter.ID)
	}
	s.metrics.sourceErrors.Add(uint64(request.SourceErrorCount))
	if request.SourceErrorCount > 0 {
		s.logger.Warn("source_errors", "count", request.SourceErrorCount)
	}
	started := time.Now()
	// Nil stays nil (older agent — keep existing rows); a health-aware agent
	// always replaces its machine's rows, even with zero sources.
	var sources []database.AgentSourceHealth
	if request.Sources != nil {
		sources = make([]database.AgentSourceHealth, 0, len(request.Sources))
		for _, source := range request.Sources {
			sources = append(sources, database.AgentSourceHealth{
				Adapter:            source.Adapter,
				SourceHash:         source.IdentityHash,
				LastSuccessfulSync: source.LastSuccessfulSync,
				LastErrorClass:     source.LastErrorClass,
			})
		}
	}
	if err := s.store.RecordHeartbeat(r.Context(), database.AgentHeartbeat{
		MachineID:        request.MachineID,
		AgentVersion:     request.AgentVersion,
		OperatingSystem:  request.OperatingSystem,
		Architecture:     request.Architecture,
		Adapters:         adapterIDs,
		SourceCount:      request.SourceCount,
		SourceErrorCount: request.SourceErrorCount,
		Sources:          sources,
	}); err != nil {
		s.metrics.observeDB(started)
		s.logger.Error("heartbeat_failed")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not persist heartbeat"})
		return
	}
	s.metrics.observeDB(started)
	s.invalidateOverviewCache()
	s.logger.Info("heartbeat_recorded", "source_errors", request.SourceErrorCount)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) machines(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	result, err := s.store.MachinesAt(r.Context(), time.Now())
	s.metrics.observeDB(started)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) quotasAPI(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	statuses := s.store.QuotaStatus(r.Context(), time.Now())
	s.metrics.observeDB(started)
	if statuses == nil {
		statuses = []database.QuotaStatus{}
	}
	writeJSON(w, http.StatusOK, statuses)
}

// ——— Login form ———

type loginPageData struct {
	PageChrome
	Next  string
	Error string
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if !s.passwordAccountConfigured() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	page := loginPageData{PageChrome: s.pageChrome("Tokemon · Sign in", "login", false), Next: safeRedirectPath(r.URL.Query().Get("next"))}
	s.renderLogin(w, http.StatusOK, page)
}

func (s *Server) renderLogin(w http.ResponseWriter, status int, page loginPageData) {
	if err := s.template.ExecuteTemplate(w, "login", page); err != nil {
		s.logger.Error("login_render_failed", "err", err.Error())
	}
}

// renderLoginMessage renders the login form with an error, preserving the
// next target from the query — used before the POST body has been parsed.
func (s *Server) renderLoginMessage(w http.ResponseWriter, status int, csrfToken, next, message string) {
	w.WriteHeader(status)
	chrome := s.pageChrome("Tokemon · Sign in", "login", false)
	chrome.CSRFToken = csrfToken
	s.renderLogin(w, status, loginPageData{PageChrome: chrome, Next: safeRedirectPath(next), Error: message})
}

// maxLoginFormBytes bounds the login/logout POST bodies; the forms carry a
// handful of short fields.
const maxLoginFormBytes = 8 << 10

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.passwordAccountConfigured() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginFormBytes)
	remote := loginLimitKey(r)
	// Parse the (bounded) form first — a later FormValue would consume the
	// body and swallow the size error — then the throttle, so a locked-out
	// client never reaches bcrypt.
	if err := r.ParseForm(); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.renderLoginMessage(w, http.StatusRequestEntityTooLarge, s.csrfToken(), r.URL.Query().Get("next"), "Sign-in form is too large.")
			return
		}
		s.renderLoginMessage(w, http.StatusBadRequest, s.csrfToken(), r.URL.Query().Get("next"), "Could not read the sign-in form.")
		return
	}
	page := loginPageData{PageChrome: s.pageChrome("Tokemon · Sign in", "login", false), Next: safeRedirectPath(r.Form.Get("next"))}
	render := func(status int, message string) {
		page.Error = message
		w.WriteHeader(status)
		s.renderLogin(w, status, page)
	}
	if blocked, retryAfter := s.loginThrottle.check(remote); blocked {
		seconds := int(retryAfter / time.Second)
		if retryAfter%time.Second != 0 {
			seconds++
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		render(http.StatusTooManyRequests, "Too many failed attempts. Try again in "+strconv.Itoa(seconds)+" seconds.")
		return
	}
	if !s.requireSameOrigin(r) || !s.validCSRFToken(r.Form.Get("csrf_token")) {
		s.logger.Warn("login_rejected", "reason", "csrf")
		render(http.StatusForbidden, "Sign-in rejected: the request did not come from this page. Reload and try again.")
		return
	}
	username := strings.TrimSpace(r.Form.Get("username"))
	page.Next = safeRedirectPath(r.Form.Get("next"))
	if !s.verifyDashboardPassword(username, r.Form.Get("password")) {
		s.loginThrottle.recordFailure(remote)
		s.logger.Warn("login_failed")
		render(http.StatusUnauthorized, "Invalid username or password.")
		return
	}
	s.loginThrottle.recordSuccess(remote)
	now := time.Now()
	value, err := mintSessionCookieValue(s.sessionSecret, now, now.Add(sessionTTL),
		credentialFingerprint(username, s.config.DashboardPasswordHash))
	if err != nil {
		s.logger.Error("session_mint_failed", "err", err.Error())
		render(http.StatusInternalServerError, "Could not start a session. Try again.")
		return
	}
	http.SetCookie(w, sessionCookie(value, int(sessionTTL/time.Second), s.cookieSecure(r)))
	s.logger.Info("login_succeeded")
	http.Redirect(w, r, page.Next, http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if !s.passwordAccountConfigured() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginFormBytes)
	if !s.requireSameOrigin(r) || !s.validCSRFToken(r.FormValue("csrf_token")) {
		s.logger.Warn("logout_rejected", "reason", "csrf")
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "logout rejected: same-origin request required"})
		return
	}
	http.SetCookie(w, sessionCookie("", -1, s.cookieSecure(r)))
	if _, err := r.Cookie(sessionCookieName); err != nil {
		// No session to end (Basic/Bearer user clicked the affordance): send
		// them back where they were instead of the login page.
		s.logger.Info("logout_no_session")
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.logger.Info("logout")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) catalogAPI(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	result, err := s.cachedCatalog(r.Context())
	s.metrics.observeDB(started)
	if err != nil {
		if clientGone(err) {
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) modelsAPI(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	result, err := s.cachedCatalog(r.Context())
	s.metrics.observeDB(started)
	if err != nil {
		if clientGone(err) {
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result.Models)
}

func (s *Server) sessionsAPI(w http.ResponseWriter, r *http.Request) {
	page, pageSize := sessionPaginationFromRequest(r)
	started := time.Now()
	result, err := s.cachedSessionsPage(r.Context(), analyticsQueryFromRequest(r), "when", "desc", page, pageSize)
	s.metrics.observeDB(started)
	if err != nil {
		if clientGone(err) {
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(result.Total))
	w.Header().Set("X-Page", strconv.Itoa(result.Page))
	w.Header().Set("X-Page-Size", strconv.Itoa(result.PageSize))
	w.Header().Set("X-Total-Pages", strconv.Itoa(result.TotalPages))
	writeJSON(w, http.StatusOK, result.Rows)
}

func (s *Server) sessionsExport(w http.ResponseWriter, r *http.Request) {
	query := analyticsQueryFromRequest(r)
	started := time.Now()
	result, err := s.store.ExportSessions(r.Context(), query, "when", "desc")
	s.metrics.observeDB(started)
	if err != nil {
		if clientGone(err) {
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if r.URL.Query().Get("format") == "csv" {
		writeSessionsCSV(w, query, result)
		return
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	payload = append(payload, '\n')
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="tokemon-sessions-`+query.Period+`.json"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

// csvFormulaSafe prefixes spreadsheet-dangerous cells with a single quote so
// a value that starts with a formula character is stored as text instead of
// being executed by Excel, LibreOffice, or Google Sheets when the export is
// opened. Text columns carry provider- and user-influenced values (session
// IDs, machine names, project labels, model names), so every text cell goes
// through this; numeric and boolean cells cannot start with these characters
// and are written raw.
func csvFormulaSafe(value string) string {
	if value == "" {
		return ""
	}
	switch value[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + value
	}
	return value
}

// writeSessionsCSV streams the session table as RFC 4180 CSV. Unknown values
// render as empty cells — never as zero — matching the dashboard vocabulary.
func writeSessionsCSV(w http.ResponseWriter, query database.AnalyticsQuery, records []database.SessionRecord) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="tokemon-sessions-`+query.Period+`.csv"`)
	w.WriteHeader(http.StatusOK)
	writer := csv.NewWriter(w)
	defer writer.Flush()
	if err := writer.Write([]string{"session_id", "timestamp", "machine", "provider", "model", "tool", "project", "input_tokens", "output_tokens", "total_tokens", "cost", "cost_estimated", "duration_ms", "accuracy", "events"}); err != nil {
		return
	}
	for _, record := range records {
		row := []string{
			csvFormulaSafe(record.SessionID),
			record.Timestamp,
			csvFormulaSafe(record.Machine),
			csvFormulaSafe(record.Provider),
			csvFormulaSafe(record.Model),
			csvFormulaSafe(record.Tool),
			csvFormulaSafe(record.Project),
			nullInt64Cell(record.InputTokens),
			nullInt64Cell(record.OutputTokens),
			nullInt64Cell(record.TotalTokens),
			nullCostCell(record.Cost),
			strconv.FormatBool(record.CostEstimated),
			nullInt64Cell(record.DurationMS),
			string(record.Accuracy),
			strconv.FormatInt(record.Events, 10),
		}
		if err := writer.Write(row); err != nil {
			return
		}
	}
}

func nullInt64Cell(value *int64) string {
	if value == nil {
		return ""
	}
	return strconv.FormatInt(*value, 10)
}

func nullCostCell(value *float64) string {
	if value == nil {
		return ""
	}
	return strconv.FormatFloat(*value, 'f', -1, 64)
}

// sharedComputeTimeout bounds a cache compute that is detached from the
// request that triggered it. A client disconnect (a reload, the dashboard's
// auto-refresh, a closed tab) must not abort a compute other requests are
// waiting on; the timeout still bounds a pathological compute.
const sharedComputeTimeout = 30 * time.Second

// detachedComputeContext keeps the request's values but drops its
// cancellation, so a shared cache compute survives the triggering client
// going away.
func detachedComputeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), sharedComputeTimeout)
}

// clientGone reports whether an error is the request context ending (client
// disconnected or deadline exceeded). Handlers must not write a 5xx for it:
// there is no client left to read it, and it is not a server failure.
func clientGone(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (s *Server) evolution(w http.ResponseWriter, r *http.Request) {
	result, err := s.cachedEvolution(r.Context())
	if err != nil {
		if clientGone(err) {
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	etag := fmt.Sprintf(`"ev-%d-%d-%d-%d-%d-%d"`,
		result.LifetimeTokens,
		result.Composition.InputTokens,
		result.Composition.UncachedInputTokens,
		result.Composition.CachedInputTokens,
		result.Composition.OutputTokens,
		result.Composition.UnclassifiedTokens,
	)
	w.Header().Set("ETag", etag)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// evolutionCacheTTL bounds how long cached evolution data may be served without
// any ingest writing to the store.
const evolutionCacheTTL = time.Minute

// evolutionMinRefreshInterval coalesces ingest invalidations into at most one
// background refresh per window while readers keep serving the last good value.
const evolutionMinRefreshInterval = 2 * time.Second

func (s *Server) cachedEvolution(ctx context.Context) (evolutionResponse, error) {
	for {
		s.evolutionMu.Lock()
		if cached := s.evolutionCache; cached != nil {
			stale := time.Since(cached.fetched) >= evolutionCacheTTL ||
				(!cached.refreshDue.IsZero() && !time.Now().Before(cached.refreshDue))
			if stale && s.evolutionReady == nil {
				s.startEvolutionRefreshLocked()
			}
			result, err := cached.result, cached.err
			s.evolutionMu.Unlock()
			return result, err
		}
		if ready := s.evolutionReady; ready != nil {
			s.evolutionMu.Unlock()
			select {
			case <-ready:
				continue
			case <-ctx.Done():
				return evolutionResponse{}, ctx.Err()
			}
		}
		// Remember the invalidation generation so a compute that started before
		// an ingest cannot install its stale result after the ingest completed.
		generation := s.evolutionGeneration
		s.evolutionReady = make(chan struct{})
		s.evolutionMu.Unlock()

		computeCtx, cancel := detachedComputeContext(ctx)
		result, err := s.computeEvolution(computeCtx)
		cancel()

		s.evolutionMu.Lock()
		ready := s.evolutionReady
		s.evolutionReady = nil
		if s.evolutionGeneration == generation && !clientGone(err) {
			s.evolutionCache = &evolutionCacheEntry{result: result, err: err, fetched: time.Now()}
		}
		s.evolutionMu.Unlock()
		close(ready)
		return result, err
	}
}

func (s *Server) startEvolutionRefreshLocked() {
	if !s.tryStartBackground() {
		return
	}
	s.evolutionRefreshes.Add(1)
	generation := s.evolutionGeneration
	ready := make(chan struct{})
	s.evolutionReady = ready
	go func() {
		defer s.bgWG.Done()
		ctx, cancel := context.WithTimeout(s.backgroundContext(), sharedComputeTimeout)
		defer cancel()
		result, err := s.computeEvolution(ctx)

		s.evolutionMu.Lock()
		if s.evolutionGeneration == generation && !clientGone(err) {
			s.evolutionCache = &evolutionCacheEntry{result: result, err: err, fetched: time.Now()}
		}
		s.evolutionReady = nil
		s.evolutionMu.Unlock()
		close(ready)
	}()
}

func (s *Server) computeEvolution(ctx context.Context) (evolutionResponse, error) {
	started := time.Now()
	snapshot, err := s.store.Evolution(ctx)
	s.metrics.observeDB(started)
	if err != nil {
		return evolutionResponse{}, err
	}
	started = time.Now()
	composition, err := s.store.TokenComposition(ctx)
	s.metrics.observeDB(started)
	if err != nil {
		return evolutionResponse{}, err
	}
	return evolutionResponse{Snapshot: snapshot, Composition: composition}, nil
}

func (s *Server) invalidateEvolutionCache() {
	now := time.Now()
	s.evolutionMu.Lock()
	s.evolutionGeneration++
	if s.evolutionCache != nil {
		due := now
		if windowEnd := s.evolutionCache.fetched.Add(evolutionMinRefreshInterval); windowEnd.After(now) {
			due = windowEnd
		}
		s.evolutionCache.refreshDue = due
	}
	s.evolutionMu.Unlock()
}

// overviewCacheTTL bounds how long cached dashboard overview data may be
// served without any ingest writing to the store; the ingest path still
// invalidates immediately. The dashboard auto-refreshes and each Overview
// render costs several full-table aggregation scans, so repeat renders within
// the TTL reuse one compute.
const overviewCacheTTL = time.Minute

func (s *Server) cachedOverview(ctx context.Context) (database.Overview, time.Time, error) {
	entry := s.cachedOverviewEntry(ctx)
	return entry.result, entry.fetched, entry.err
}

// cachedOverviewEntry returns the overview entry currently being served (or the
// one computed for a cold cache) as a snapshot. The result and the page always
// come from the same entry, so a reader cannot pair a fresh overview with a
// stale page or the reverse.
func (s *Server) cachedOverviewEntry(ctx context.Context) overviewCacheEntry {
	for {
		s.overviewMu.Lock()
		if cached := s.overviewCache; cached != nil {
			// Stale-while-revalidate: serve whatever we have immediately and
			// refresh in the background when it is past the TTL. A reader never
			// waits on a recompute, even right after an ingest invalidated it.
			if time.Since(cached.fetched) >= overviewCacheTTL && s.overviewReady == nil {
				s.startOverviewRefreshLocked()
			}
			entry := *cached
			s.overviewMu.Unlock()
			return entry
		}
		if ready := s.overviewReady; ready != nil {
			s.overviewMu.Unlock()
			select {
			case <-ready:
				continue
			case <-ctx.Done():
				return overviewCacheEntry{err: ctx.Err()}
			}
		}
		// No cache yet: the first render computes synchronously so it has data.
		// Remember the invalidation generation so a compute that started before
		// an ingest cannot install its stale result after the ingest completed.
		generation := s.overviewGeneration
		s.overviewReady = make(chan struct{})
		s.overviewMu.Unlock()

		computeCtx, cancel := detachedComputeContext(ctx)
		result, err := s.store.Overview(computeCtx)
		cancel()

		s.overviewMu.Lock()
		ready := s.overviewReady
		s.overviewReady = nil
		fetched := time.Time{}
		if s.overviewGeneration == generation && !clientGone(err) {
			fetched = time.Now()
			s.overviewCache = &overviewCacheEntry{result: result, err: err, fetched: fetched}
		}
		s.overviewMu.Unlock()
		close(ready)
		return overviewCacheEntry{result: result, err: err, fetched: fetched}
	}
}

// startOverviewRefreshLocked starts one background overview refresh. The
// caller holds overviewMu; readers keep serving the cached result while it
// runs, and the generation guard drops a result that an ingest superseded.
//
// The page is rendered before the entry is published, so the fresh overview and
// its pre-rendered HTML become visible together. Publishing first and rendering
// after (the previous shape) left a window where a GET / saw the new entry,
// missed the old page, and rebuilt it on the request path — including a
// Sessions query that queued behind the analytics refresh (756-934ms observed,
// docs/analytics-latency.md). Rendering first closes that window structurally:
// there is no state in which a fresh entry exists without its page. The cost is
// that readers keep the previous entry for the render's duration, which is the
// same stale-while-revalidate bargain they already accept for the refresh.
func (s *Server) startOverviewRefreshLocked() {
	if !s.tryStartBackground() {
		return
	}
	generation := s.overviewGeneration
	ready := make(chan struct{})
	s.overviewReady = ready
	go func() {
		defer s.bgWG.Done()
		ctx, cancel := context.WithTimeout(s.backgroundContext(), sharedComputeTimeout)
		defer cancel()
		result, err := s.store.Overview(ctx)

		var html []byte
		var etag string
		if err == nil {
			var buildErr error
			if html, buildErr = s.renderDashboardHTML(ctx, result); buildErr != nil {
				// Publish the entry without a page: the next reader falls back
				// to a request-path build instead of serving stale bytes.
				html = nil
				if !clientGone(buildErr) {
					s.logger.Warn("dashboard_prebuild_failed")
				}
			} else {
				etag = dashboardETag(html)
			}
		}

		s.overviewMu.Lock()
		var fetched time.Time
		if s.overviewGeneration == generation && !clientGone(err) {
			fetched = time.Now()
			s.overviewCache = &overviewCacheEntry{result: result, err: err, fetched: fetched, html: html, etag: etag}
		}
		s.overviewReady = nil
		s.overviewMu.Unlock()
		close(ready)
	}()
}

// WarmCaches pre-computes the overview (with its pre-rendered dashboard),
// the default analytics window, and evolution, strictly in that order: the
// scans contend on the same SQLite database, and running them concurrently
// on a cold page cache slows the overview compute the first / joins from
// ~0.7s to ~13s (live regression, 2026-11-07).
// The compute is detached from the caller's context — a shutdown signal
// racing the warm must not leave the first request cold — and bounded by
// sharedComputeTimeout per compute while honoring Server.Close().
func (s *Server) WarmCaches(_ context.Context) {
	if !s.tryStartBackground() {
		return
	}
	defer s.bgWG.Done()
	computeCtx, cancel := context.WithTimeout(s.backgroundContext(), sharedComputeTimeout)
	defer cancel()
	if result, fetched, err := s.cachedOverview(computeCtx); err != nil {
		s.logger.Debug("warmup_overview_failed", "error", err.Error())
	} else if _, err := s.buildDashboardHTML(computeCtx, result, fetched); err != nil {
		s.logger.Debug("warmup_dashboard_failed", "error", err.Error())
	}
	query := database.AnalyticsQuery{Period: "30d", Dimension: "projects", Now: time.Now().UTC()}
	if _, err := s.cachedAnalytics(computeCtx, query); err != nil {
		s.logger.Debug("warmup_analytics_failed", "error", err.Error())
	}
	if _, err := s.cachedEvolution(computeCtx); err != nil {
		s.logger.Debug("warmup_evolution_failed", "error", err.Error())
	}
}

func (s *Server) invalidateOverviewCache() {
	s.overviewMu.Lock()
	s.overviewGeneration++
	if s.overviewCache != nil {
		// Keep the last good result so readers are served immediately; the
		// zero fetched time marks it stale and the next request refreshes it.
		// The rendered page rides along on the entry, so the stale overview and
		// its page stay a pair until the refresh replaces both.
		s.overviewCache.fetched = time.Time{}
	}
	s.overviewMu.Unlock()
}

func (s *Server) dropOverviewHTMLCache() {
	s.overviewMu.Lock()
	s.overviewGeneration++
	if s.overviewCache != nil {
		s.overviewCache.html = nil
		s.overviewCache.etag = ""
		s.overviewCache.fetched = time.Time{}
	}
	s.overviewMu.Unlock()
}

// analyticsCacheTTL bounds how long a cached Analytics result may be served
// without any ingest writing to the store; the ingest path invalidates
// immediately. Each Analytics render costs several passes over the window's
// rows, so repeat renders of one filter within the TTL reuse one compute.
const analyticsCacheTTL = time.Minute

// analyticsMinRefreshInterval coalesces ingest invalidations: an entry
// invalidated within this interval of its last refresh defers its next
// refresh until the interval elapses, so a burst of N ingests starts at most
// one recompute instead of N. The trade-off is that after an ingest a cached
// result can be at most this much staler than the store; readers still serve
// the last good result immediately and never wait on a recompute. Without an
// invalidation, analyticsCacheTTL still bounds staleness.
const analyticsMinRefreshInterval = 2 * time.Second

// analyticsCacheKey identifies the filter combination a cached result was
// computed for. Now is deliberately excluded: it moves per request and the
// TTL already bounds how far the cached window may lag.
func analyticsCacheKey(query database.AnalyticsQuery) string {
	return query.Period + "\x1f" + query.Dimension + "\x1f" + query.From + "\x1f" + query.To + "\x1f" + query.Machine + "\x1f" + query.Provider + "\x1f" + query.Model + "\x1f" + query.Tool + "\x1f" + query.Project + "\x1f" + query.Search
}

func (s *Server) cachedAnalytics(ctx context.Context, query database.AnalyticsQuery) (database.Analytics, error) {
	key := analyticsCacheKey(query)
	for {
		s.analyticsMu.Lock()
		if entry, ok := s.analyticsCache[key]; ok && entry.ready != nil && !entry.hasResult {
			// A first-ever compute is in flight and there is no last good
			// result to serve, so the reader waits for it.
			s.analyticsMu.Unlock()
			select {
			case <-entry.ready:
				continue
			case <-ctx.Done():
				return database.Analytics{}, ctx.Err()
			}
		}
		if entry, ok := s.analyticsCache[key]; ok && entry.hasResult {
			// Stale-while-revalidate: serve whatever we have immediately and
			// refresh in the background when it is past the TTL or past the
			// coalescing deadline an ingest set. A reader never waits on a
			// recompute, even right after an ingest invalidated it.
			stale := time.Since(entry.fetched) >= analyticsCacheTTL ||
				(!entry.refreshDue.IsZero() && !time.Now().Before(entry.refreshDue))
			if stale && entry.ready == nil {
				s.startAnalyticsRefreshLocked(key, entry)
			}
			entry.lastUse = time.Now()
			result, err := entry.result, entry.err
			s.analyticsMu.Unlock()
			return result, err
		}
		if s.analyticsCache == nil {
			s.analyticsCache = make(map[string]*analyticsCacheEntry)
		}
		computing := &analyticsCacheEntry{query: query, ready: make(chan struct{})}
		s.analyticsCache[key] = computing
		s.evictAnalyticsEntriesLocked()
		generation := s.analyticsGeneration
		s.analyticsMu.Unlock()

		computeCtx, cancel := detachedComputeContext(ctx)
		result, err := s.store.Analytics(computeCtx, query)
		cancel()

		s.analyticsMu.Lock()
		if s.analyticsGeneration == generation && !clientGone(err) {
			s.analyticsCache[key] = &analyticsCacheEntry{query: query, result: result, err: err, fetched: time.Now(), hasResult: true, lastUse: time.Now()}
		} else {
			delete(s.analyticsCache, key)
		}
		ready := computing.ready
		s.analyticsMu.Unlock()
		close(ready)
		return result, err
	}
}

// startAnalyticsRefreshLocked starts one background analytics refresh for one
// filter key. The caller holds analyticsMu; readers keep serving the cached
// result while it runs, and the generation guard drops a result that an
// ingest superseded.
func (s *Server) startAnalyticsRefreshLocked(key string, entry *analyticsCacheEntry) {
	if !s.tryStartBackground() {
		return
	}
	s.analyticsRefreshes.Add(1)
	generation := s.analyticsGeneration
	query := entry.query
	query.Now = time.Now().UTC()
	ready := make(chan struct{})
	entry.ready = ready
	go func() {
		defer s.bgWG.Done()
		ctx, cancel := context.WithTimeout(s.backgroundContext(), sharedComputeTimeout)
		defer cancel()
		result, err := s.store.Analytics(ctx, query)

		s.analyticsMu.Lock()
		if s.analyticsGeneration == generation && !clientGone(err) {
			s.analyticsCache[key] = &analyticsCacheEntry{query: query, result: result, err: err, fetched: time.Now(), hasResult: true, lastUse: time.Now()}
		}
		entry.ready = nil
		s.analyticsMu.Unlock()
		close(ready)
	}()
}

// analyticsCacheMaxEntries bounds how many filter combinations the analytics
// cache retains. SWR keeps the last result per filter for the process
// lifetime, so an unbounded map would grow with every distinct filter a
// client tries.
const analyticsCacheMaxEntries = 64

// evictAnalyticsEntriesLocked drops the least recently served entries until
// the cache is within its bound. Entries with a compute in flight are never
// evicted: their waiters hold the ready channel and the generation guard
// expects them to stay installed. If every entry is in flight, eviction
// waits for the next insert.
func (s *Server) evictAnalyticsEntriesLocked() {
	for len(s.analyticsCache) > analyticsCacheMaxEntries {
		victim := ""
		var oldest time.Time
		for key, entry := range s.analyticsCache {
			if entry.ready != nil {
				continue
			}
			if victim == "" || entry.lastUse.Before(oldest) {
				victim, oldest = key, entry.lastUse
			}
		}
		if victim == "" {
			return
		}
		delete(s.analyticsCache, victim)
	}
}

// invalidateAnalyticsCache marks every analytics entry stale after an ingest.
// Ingests arrive in bursts and each refresh costs a full scan, so an entry
// refreshed within analyticsMinRefreshInterval defers its next refresh to the
// window's end instead of refreshing on the next read: a burst of N
// invalidations coalesces into at most one recompute per window. The deadline
// derives from the entry's last refresh, so later invalidations inside the
// same window never extend it, and an entry last refreshed before the window
// stays due on the next read exactly as before. Entries with a compute in
// flight are left alone: their generation guard drops a result this
// invalidation superseded, and the already-elapsed deadline brings the next
// reader back.
func (s *Server) invalidateAnalyticsCache() {
	now := time.Now()
	s.analyticsMu.Lock()
	s.analyticsGeneration++
	for _, entry := range s.analyticsCache {
		if entry.ready != nil {
			continue
		}
		due := now
		if windowEnd := entry.fetched.Add(analyticsMinRefreshInterval); windowEnd.After(now) {
			due = windowEnd
		}
		entry.refreshDue = due
	}
	s.analyticsMu.Unlock()
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	result, _, err := s.cachedOverview(r.Context())
	s.metrics.observeDB(started)
	if err != nil {
		if clientGone(err) {
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	entry := s.cachedOverviewEntry(r.Context())
	s.metrics.observeDB(started)
	if entry.err != nil {
		if clientGone(entry.err) {
			return
		}
		http.Error(w, entry.err.Error(), http.StatusInternalServerError)
		return
	}
	if entry.fetched.IsZero() || time.Since(entry.fetched) >= overviewCacheTTL {
		w.Header().Set("X-Tokemon-Refreshing", "1")
	}
	// The page is cached on the overview entry it was rendered from, so a warm
	// request writes pre-rendered bytes instead of repeating the machine,
	// session, and alias queries plus the full template render.
	html := entry.html
	etag := entry.etag
	if len(html) == 0 {
		var err error
		html, err = s.buildDashboardHTML(r.Context(), entry.result, entry.fetched)
		if err != nil {
			if clientGone(err) {
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		etag = dashboardETag(html)
	}
	if etag != "" {
		w.Header().Set("ETag", etag)
		if etagMatches(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.Write(html)
}

// buildDashboardHTML renders the dashboard page and attaches it to the overview
// entry it was built from, so the next reader reuses the bytes. A page built
// from an entry that a refresh has since replaced is served but not cached: the
// replacement carries its own page.
func (s *Server) buildDashboardHTML(ctx context.Context, result database.Overview, overviewFetched time.Time) ([]byte, error) {
	html, err := s.renderDashboardHTML(ctx, result)
	if err != nil {
		return nil, err
	}
	etag := dashboardETag(html)
	// A zero fetched time marks the stale entry that an ingest invalidated;
	// attaching there would let one request's page outlive the refresh that is
	// already replacing it, and the refresh pre-renders the successor anyway.
	if !overviewFetched.IsZero() {
		s.overviewMu.Lock()
		if cached := s.overviewCache; cached != nil && cached.fetched.Equal(overviewFetched) && len(cached.html) == 0 {
			cached.html = html
			cached.etag = etag
		}
		s.overviewMu.Unlock()
	}
	return html, nil
}

// renderDashboardHTML assembles the dashboard page from a computed overview. It
// deliberately does not publish the bytes: the caller owns the entry they
// belong to, which is what keeps the page and its overview from diverging.
func (s *Server) renderDashboardHTML(ctx context.Context, result database.Overview) ([]byte, error) {
	aliases, err := s.store.DisplayAliases(ctx)
	if err != nil {
		return nil, err
	}
	modelAliases, machineAliases := aliasMaps(aliases)
	sessions, err := s.store.SessionsWithLimit(ctx, database.AnalyticsQuery{Period: "30d", Dimension: "projects", Now: time.Now().UTC()}, "when", "desc", 8)
	if err != nil {
		return nil, err
	}
	quotas := quotaViews(s.store.QuotaStatus(ctx, time.Now()))
	// The pre-rendered page is served to every authenticated user, so the
	// logout affordance keys on the password account being configured, not
	// on the per-request auth method. POST /logout is a no-op redirect for
	// credentials-authenticated users.
	page := dashboardPage{PageChrome: s.pageChrome("Tokemon · Overview", "overview", true), Overview: result, ModelAliases: modelAliases, MachineAliases: machineAliases, Machines: result.Machines, Sessions: sessions, Freshness: dataFreshnessFor(result.NewestEvent, time.Now()), Quotas: quotas}
	var buffer bytes.Buffer
	if err := s.template.ExecuteTemplate(&buffer, "dashboard", page); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func formatSubDollarCost(amount float64) string {
	if amount >= 1 {
		return money(amount)
	}
	return fmt.Sprintf("$%.4f", amount)
}

func pricingHasKnownRates(pricing catalog.Pricing) bool {
	return pricing.Mode == "standard" && pricing.InputPricePerMillion != nil && pricing.OutputPricePerMillion != nil
}

func catalogWhatIfEstimate(pricing catalog.Pricing, recent30d database.ComponentUsageTotals) (priced bool, inEff, crEff, cwEff, outEff, cost float64, note string) {
	if !pricingHasKnownRates(pricing) {
		return false, 0, 0, 0, 0, 0, ""
	}
	inEff = *pricing.InputPricePerMillion
	outEff = *pricing.OutputPricePerMillion
	crEff = inEff
	cwEff = inEff
	cacheFallback := false
	if pricing.CacheReadPricePerMillion != nil {
		crEff = *pricing.CacheReadPricePerMillion
	} else if recent30d.CacheReadTokens > 0 {
		cacheFallback = true
	}
	if pricing.CacheWritePricePerMillion != nil {
		cwEff = *pricing.CacheWritePricePerMillion
	} else if recent30d.CacheWriteTokens > 0 {
		cacheFallback = true
	}
	if cacheFallback {
		note = "cache @ input rate"
	}
	if recent30d.ComponentEvents > 0 {
		cost = (float64(recent30d.InputTokens)*inEff +
			float64(recent30d.CacheReadTokens)*crEff +
			float64(recent30d.CacheWriteTokens)*cwEff +
			float64(recent30d.OutputTokens)*outEff) / 1_000_000.0
	}
	return true, inEff, crEff, cwEff, outEff, cost, note
}

func (s *Server) catalogPage(w http.ResponseWriter, r *http.Request) {
	if !s.config.EnableTokedex {
		// The catalog UI is opt-in (TOKEMON_ENABLE_TOKEDEX, see
		// docs/tokedex-roadmap.md): hide the route instead of rendering a
		// page the release does not consider finished. The check runs after
		// dashboard auth, so unauthenticated requests still see 401 and the
		// read API (/api/v1/catalog) is unaffected.
		http.NotFound(w, r)
		return
	}
	snapshot, err := s.cachedCatalog(r.Context())
	if err != nil {
		if clientGone(err) {
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	byTier := make(map[string][]database.CatalogEntry)
	whatIfRows := make([]tokedexWhatIfRow, 0, len(snapshot.Models))
	whatIfByID := make(map[string]tokedexWhatIfRow, len(snapshot.Models))
	hasUsage := snapshot.Recent30d.ComponentEvents > 0
	for _, model := range snapshot.Models {
		priced, inEff, crEff, cwEff, outEff, cost, note := catalogWhatIfEstimate(model.Pricing, snapshot.Recent30d)
		row := tokedexWhatIfRow{
			ID:            model.ID,
			DisplayName:   model.DisplayName,
			Provider:      model.Provider,
			Tier:          model.Tier,
			Priced:        priced,
			InputRate:     model.Pricing.InputPricePerMillion,
			CacheReadRate: model.Pricing.CacheReadPricePerMillion,
			OutputRate:    model.Pricing.OutputPricePerMillion,
			InputEff:      inEff,
			CacheReadEff:  crEff,
			CacheWriteEff: cwEff,
			OutputEff:     outEff,
			Cost:          cost,
			Note:          note,
			DetailURL:     tokedexDetailURL(model.ID),
		}
		if priced {
			row.InputRateAttr = strconv.FormatFloat(inEff, 'f', -1, 64)
			row.CacheReadRateAttr = strconv.FormatFloat(crEff, 'f', -1, 64)
			row.CacheWriteRateAttr = strconv.FormatFloat(cwEff, 'f', -1, 64)
			row.OutputRateAttr = strconv.FormatFloat(outEff, 'f', -1, 64)
			if hasUsage {
				row.CostLabel = formatSubDollarCost(cost)
			} else {
				row.CostLabel = "—"
			}
		} else {
			row.CostLabel = "Unknown"
		}
		byTier[model.Tier] = append(byTier[model.Tier], model)
		whatIfRows = append(whatIfRows, row)
		whatIfByID[model.ID] = row
	}
	sort.SliceStable(whatIfRows, func(i, j int) bool {
		if whatIfRows[i].Priced != whatIfRows[j].Priced {
			return whatIfRows[i].Priced
		}
		if whatIfRows[i].Priced && whatIfRows[i].Cost != whatIfRows[j].Cost {
			return whatIfRows[i].Cost < whatIfRows[j].Cost
		}
		return whatIfRows[i].DisplayName < whatIfRows[j].DisplayName
	})
	page := catalogPageData{
		PageChrome:             s.pageChrome("Tokemon · Tokedex", "tokedex", false),
		Tiers:                  make([]catalogTier, 0, len(snapshot.Tiers)),
		Uncataloged:            snapshot.Uncataloged,
		ObservedTokens:         snapshot.ObservedTokens,
		PricedTokens:           snapshot.PricedTokens,
		CatalogedUnknownTokens: snapshot.CatalogedUnknownTokens,
		UncatalogedModels:      snapshot.UncatalogedModels,
		Recent30d:              snapshot.Recent30d,
		WhatIfRows:             whatIfRows,
		WhatIfByID:             whatIfByID,
		WhatIfHasUsage:         hasUsage,
	}
	for _, tier := range snapshot.Tiers {
		models := byTier[tier]
		if len(models) == 0 {
			continue
		}
		page.Tiers = append(page.Tiers, catalogTier{Name: tier, Models: models})
	}
	page.TierLegend = catalogTierLegend(page.Tiers)
	page.Providers = catalogProviders(page.Tiers)
	page.ModelCount = len(snapshot.Models) + len(snapshot.Uncataloged)
	if err := s.template.ExecuteTemplate(w, "catalog", page); err != nil {
		return
	}
}

func containsBackslashOrControl(value string) bool {
	for _, r := range value {
		if r == '\\' || unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func (s *Server) tokedexDetailPage(w http.ResponseWriter, r *http.Request) {
	if !s.config.EnableTokedex {
		http.NotFound(w, r)
		return
	}
	modelID := strings.TrimSpace(r.PathValue("model"))
	if modelID == "" || modelID == "." || modelID == ".." || len([]rune(modelID)) > usage.MaxModelLength || containsBackslashOrControl(modelID) {
		http.NotFound(w, r)
		return
	}
	started := time.Now()
	detail, err := s.store.TokedexModelDetail(r.Context(), modelID)
	s.metrics.observeDB(started)
	if err != nil {
		if errors.Is(err, database.ErrModelNotFound) {
			http.NotFound(w, r)
			return
		}
		if clientGone(err) {
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	started = time.Now()
	aliases, err := s.store.DisplayAliases(r.Context())
	s.metrics.observeDB(started)
	if err != nil {
		if clientGone(err) {
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	modelAliases, machineAliases := aliasMaps(aliases)
	var weeklyMax int64
	for _, wk := range detail.WeeklyTrend {
		if wk.Tokens > weeklyMax {
			weeklyMax = wk.Tokens
		}
	}
	var analyticsURL, sessionsURL string
	if isDrillableBucket(detail.ID) {
		analyticsURL = "/analytics?period=all&dimension=projects&model=" + url.QueryEscape(detail.ID)
		sessionsURL = "/sessions?period=all&model=" + url.QueryEscape(detail.ID)
	}
	page := tokedexDetailPageData{
		PageChrome:      s.pageChrome("Tokemon · "+detail.DisplayName, "tokedex", false),
		Detail:          detail,
		ModelAliases:    modelAliases,
		MachineAliases:  machineAliases,
		AnalyticsURL:    analyticsURL,
		SessionsURL:     sessionsURL,
		WeeklyMaxTokens: weeklyMax,
	}
	if err := s.template.ExecuteTemplate(w, "tokedex_detail", page); err != nil {
		return
	}
}

func (s *Server) sessionsPage(w http.ResponseWriter, r *http.Request) {
	query := analyticsQueryFromRequest(r)
	sort, dir := database.NormalizeSessionSort(r.URL.Query().Get("sort"), r.URL.Query().Get("dir"))
	pageNum, pageSize := sessionPaginationFromRequest(r)
	result, err := s.cachedSessionsPage(r.Context(), query, sort, dir, pageNum, pageSize)
	if err != nil {
		if clientGone(err) {
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	analytics, err := s.store.Facets(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	aliases, err := s.store.DisplayAliases(r.Context())
	if err != nil {
		if clientGone(err) {
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	modelAliases, machineAliases := aliasMaps(aliases)
	rangeStart, rangeEnd := 0, 0
	if result.Total > 0 && len(result.Rows) > 0 {
		rangeStart = (result.Page-1)*result.PageSize + 1
		rangeEnd = rangeStart + len(result.Rows) - 1
	}
	var prevURL, nextURL string
	if result.HasPrev {
		prevURL = sessionsPageURL(query, sort, dir, result.Page-1, result.PageSize)
	}
	if result.HasNext {
		nextURL = sessionsPageURL(query, sort, dir, result.Page+1, result.PageSize)
	}
	page := sessionsPageData{
		PageChrome:     s.pageChrome("Tokemon · Sessions", "sessions", false),
		Sessions:       result.Rows,
		Filter:         query,
		Facets:         analytics,
		ModelAliases:   modelAliases,
		MachineAliases: machineAliases,
		Sorts:          sessionSortStates(r, sort, dir),
		Sort:           sort,
		Dir:            dir,
		Page:           result.Page,
		PageSize:       result.PageSize,
		TotalCount:     int64(result.Total),
		TotalPages:     result.TotalPages,
		HasPrev:        result.HasPrev,
		HasNext:        result.HasNext,
		RangeStart:     rangeStart,
		RangeEnd:       rangeEnd,
		ActiveFilters:  sessionsActiveFilters(query, sort, dir, result.PageSize, modelAliases, machineAliases),
		PrevURL:        prevURL,
		NextURL:        nextURL,
	}
	if err := s.template.ExecuteTemplate(w, "sessions", page); err != nil {
		return
	}
}

func (s *Server) sessionDetailPage(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || len([]rune(id)) > usage.MaxSessionIDLength || strings.ContainsAny(id, "/\\\r\n\t ") {
		http.NotFound(w, r)
		return
	}
	detail, err := s.store.SessionDetail(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if clientGone(err) {
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	aliases, err := s.store.DisplayAliases(r.Context())
	if err != nil {
		if clientGone(err) {
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	modelAliases, machineAliases := aliasMaps(aliases)
	page := sessionDetailPageData{
		PageChrome:     s.pageChrome("Tokemon · Session", "sessions", false),
		Detail:         detail,
		ModelAliases:   modelAliases,
		MachineAliases: machineAliases,
	}
	if err := s.template.ExecuteTemplate(w, "session_detail", page); err != nil {
		return
	}
}

// sessionSortStates builds the sortable-header state for the four server-side
// sorted columns. The active column toggles direction; the others default to
// descending. Filter parameters are preserved in each toggle URL and page
// resets to 1 so sorting restarts from the top.
func sessionSortStates(r *http.Request, activeSort, activeDir string) map[string]sessionSortState {
	states := map[string]sessionSortState{}
	for _, key := range []string{"when", "tokens", "cost", "duration"} {
		active := key == activeSort
		dir := activeDir
		indicator := ""
		ariaSort := ""
		if active {
			if dir == "desc" {
				indicator, ariaSort = "\u25bc", "descending"
			} else {
				indicator, ariaSort = "\u25b2", "ascending"
			}
			dir = map[string]string{"desc": "asc", "asc": "desc"}[dir]
		} else {
			dir = "desc"
		}
		values := r.URL.Query()
		values.Del("page")
		values.Set("sort", key)
		values.Set("dir", dir)
		states[key] = sessionSortState{URL: "?" + values.Encode(), Active: active, AriaSort: ariaSort, Indicator: indicator}
	}
	return states
}
func (s *Server) analyticsPage(w http.ResponseWriter, r *http.Request) {
	query := analyticsQueryFromRequest(r)
	started := time.Now()
	result, err := s.cachedAnalytics(r.Context(), query)
	s.metrics.observeDB(started)
	if err != nil {
		if clientGone(err) {
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	started = time.Now()
	aliases, err := s.store.DisplayAliases(r.Context())
	s.metrics.observeDB(started)
	if err != nil {
		if clientGone(err) {
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	modelAliases, machineAliases := aliasMaps(aliases)
	axisMax := analyticsNiceMax(result.MaxTokens)
	activeFilters := analyticsActiveFilters(result.Filter, modelAliases, machineAliases)
	page := analyticsPageData{
		PageChrome:        s.pageChrome("Tokemon · Analytics", "analytics", false),
		Analytics:         result,
		ModelAliases:      modelAliases,
		MachineAliases:    machineAliases,
		ExportURL:         analyticsExportURL(result.Filter, "json"),
		CSVURL:            analyticsExportURL(result.Filter, "csv"),
		PeakPoint:         analyticsPeakPoint(result.Points),
		AverageThread:     analyticsAverageThread(result.Summary),
		AxisMax:           axisMax,
		AxisTicks:         analyticsAxisTicks(axisMax),
		Insights:          analyticsInsightsWithTokedex(result, time.Now(), modelAliases, machineAliases, s.config.EnableTokedex),
		Quotas:            quotaViews(s.store.QuotaStatus(r.Context(), time.Now())),
		ActiveFilters:     activeFilters,
		ActiveFilterCount: len(activeFilters),
	}
	if err := s.template.ExecuteTemplate(w, "analytics", page); err != nil {
		return
	}
}

func (s *Server) analyticsAPI(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	result, err := s.cachedAnalytics(r.Context(), analyticsQueryFromRequest(r))
	s.metrics.observeDB(started)
	if err != nil {
		if clientGone(err) {
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) insightsRecap(ctx context.Context, result database.Insights) InsightsRecap {
	if s.recapProvider == nil {
		return InsightsRecap{Status: InsightsRecapDisabled}
	}
	text, err := s.recapProvider.Generate(ctx, result)
	if err != nil || strings.TrimSpace(text) == "" {
		return InsightsRecap{Status: InsightsRecapUnavailable}
	}
	return InsightsRecap{Status: InsightsRecapGenerated, Text: strings.TrimSpace(text)}
}

func (s *Server) insightsPage(w http.ResponseWriter, r *http.Request) {
	query := analyticsQueryFromRequest(r)
	result, err := s.cachedInsights(r.Context(), query)
	if err != nil {
		if clientGone(err) {
			return
		}
		http.Error(w, "Insights are temporarily unavailable.", http.StatusInternalServerError)
		return
	}
	facets, err := s.store.Facets(r.Context())
	if err != nil {
		http.Error(w, "Insights are temporarily unavailable.", http.StatusInternalServerError)
		return
	}
	aliases, err := s.store.DisplayAliases(r.Context())
	if err != nil {
		if clientGone(err) {
			return
		}
		http.Error(w, "Insights are temporarily unavailable.", http.StatusInternalServerError)
		return
	}
	modelAliases, machineAliases := aliasMaps(aliases)
	page := insightsPageData{
		PageChrome:     s.pageChrome("Tokemon · Insights", "insights", false),
		Insights:       result,
		Facets:         facets,
		ModelAliases:   modelAliases,
		MachineAliases: machineAliases,
		HourlyMax:      insightDistributionMax(result.Hourly),
		WeekdayMax:     insightDistributionMax(result.Weekdays),
		Recap:          s.insightsRecap(r.Context(), result),
	}
	if err := s.template.ExecuteTemplate(w, "insights", page); err != nil {
		return
	}
}

func (s *Server) insightsAPI(w http.ResponseWriter, r *http.Request) {
	result, err := s.cachedInsights(r.Context(), analyticsQueryFromRequest(r))
	if err != nil {
		if clientGone(err) {
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	payload := struct {
		database.Insights
		AIRecap InsightsRecap `json:"ai_recap"`
	}{Insights: result, AIRecap: s.insightsRecap(r.Context(), result)}
	writeJSON(w, http.StatusOK, payload)
}

const (
	catalogCacheTTL           = time.Minute
	catalogMinRefreshInterval = 2 * time.Second

	sessionsCacheTTL           = time.Minute
	sessionsMinRefreshInterval = 2 * time.Second
	sessionsCacheMaxEntries    = 64

	insightsCacheTTL           = time.Minute
	insightsMinRefreshInterval = 2 * time.Second
	insightsCacheMaxEntries    = 64
)

func (s *Server) cachedCatalog(ctx context.Context) (database.CatalogSnapshot, error) {
	for {
		s.catalogMu.Lock()
		if cached := s.catalogCache; cached != nil {
			stale := time.Since(cached.fetched) >= catalogCacheTTL ||
				(!cached.refreshDue.IsZero() && !time.Now().Before(cached.refreshDue))
			if stale && s.catalogReady == nil {
				s.startCatalogRefreshLocked()
			}
			result, err := cached.result, cached.err
			s.catalogMu.Unlock()
			return result, err
		}
		if ready := s.catalogReady; ready != nil {
			s.catalogMu.Unlock()
			select {
			case <-ready:
				continue
			case <-ctx.Done():
				return database.CatalogSnapshot{}, ctx.Err()
			}
		}
		generation := s.catalogGeneration
		s.catalogReady = make(chan struct{})
		s.catalogMu.Unlock()

		computeCtx, cancel := detachedComputeContext(ctx)
		result, err := s.store.CatalogSnapshot(computeCtx)
		cancel()

		s.catalogMu.Lock()
		ready := s.catalogReady
		s.catalogReady = nil
		if s.catalogGeneration == generation && !clientGone(err) {
			s.catalogCache = &catalogCacheEntry{result: result, err: err, fetched: time.Now()}
		}
		s.catalogMu.Unlock()
		close(ready)
		return result, err
	}
}

func (s *Server) startCatalogRefreshLocked() {
	if !s.tryStartBackground() {
		return
	}
	s.catalogRefreshes.Add(1)
	generation := s.catalogGeneration
	ready := make(chan struct{})
	s.catalogReady = ready
	go func() {
		defer s.bgWG.Done()
		ctx, cancel := context.WithTimeout(s.backgroundContext(), sharedComputeTimeout)
		defer cancel()
		result, err := s.store.CatalogSnapshot(ctx)

		s.catalogMu.Lock()
		if s.catalogGeneration == generation && !clientGone(err) {
			s.catalogCache = &catalogCacheEntry{result: result, err: err, fetched: time.Now()}
		}
		s.catalogReady = nil
		s.catalogMu.Unlock()
		close(ready)
	}()
}

func (s *Server) invalidateCatalogCache() {
	now := time.Now()
	s.catalogMu.Lock()
	s.catalogGeneration++
	if s.catalogCache != nil {
		due := now
		if windowEnd := s.catalogCache.fetched.Add(catalogMinRefreshInterval); windowEnd.After(now) {
			due = windowEnd
		}
		s.catalogCache.refreshDue = due
	}
	s.catalogMu.Unlock()
}

func sessionsCacheKey(query database.AnalyticsQuery, sort, dir string) string {
	return query.Period + "\x1f" + query.From + "\x1f" + query.To + "\x1f" + query.Machine + "\x1f" + query.Provider + "\x1f" + query.Model + "\x1f" + query.Tool + "\x1f" + query.Project + "\x1f" + query.Search + "\x1f" + sort + "\x1f" + dir
}

func sessionsPageCacheKey(query database.AnalyticsQuery, sort, dir string, page, pageSize int) string {
	base := sessionsCacheKey(query, sort, dir)
	if page <= 1 && pageSize == database.DefaultSessionsPageSize {
		return base
	}
	return base + "\x1f" + strconv.Itoa(page) + "\x1f" + strconv.Itoa(pageSize)
}

func (s *Server) cachedSessions(ctx context.Context, query database.AnalyticsQuery, sort, dir string) ([]database.SessionRecord, error) {
	result, err := s.cachedSessionsPage(ctx, query, sort, dir, 1, database.DefaultSessionsPageSize)
	return result.Rows, err
}

func (s *Server) cachedSessionsPage(ctx context.Context, query database.AnalyticsQuery, sort, dir string, page, pageSize int) (database.SessionsPageResult, error) {
	sort, dir = database.NormalizeSessionSort(sort, dir)
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = database.DefaultSessionsPageSize
	}
	if pageSize > database.MaxSessionsPageSize {
		pageSize = database.MaxSessionsPageSize
	}
	key := sessionsPageCacheKey(query, sort, dir, page, pageSize)
	for {
		s.sessionsMu.Lock()
		if entry, ok := s.sessionsCache[key]; ok && entry.ready != nil && !entry.hasResult {
			s.sessionsMu.Unlock()
			select {
			case <-entry.ready:
				continue
			case <-ctx.Done():
				return database.SessionsPageResult{}, ctx.Err()
			}
		}
		if entry, ok := s.sessionsCache[key]; ok && entry.hasResult {
			stale := time.Since(entry.fetched) >= sessionsCacheTTL ||
				(!entry.refreshDue.IsZero() && !time.Now().Before(entry.refreshDue))
			if stale && entry.ready == nil {
				s.startSessionsRefreshLocked(key, entry)
			}
			entry.lastUse = time.Now()
			result, err := entry.result, entry.err
			s.sessionsMu.Unlock()
			return result, err
		}
		if s.sessionsCache == nil {
			s.sessionsCache = make(map[string]*sessionsCacheEntry)
		}
		computing := &sessionsCacheEntry{query: query, sort: sort, dir: dir, page: page, pageSize: pageSize, ready: make(chan struct{})}
		s.sessionsCache[key] = computing
		s.evictSessionsEntriesLocked()
		generation := s.sessionsGeneration
		s.sessionsMu.Unlock()

		computeCtx, cancel := detachedComputeContext(ctx)
		result, err := s.store.SessionsPage(computeCtx, query, sort, dir, page, pageSize)
		cancel()

		s.sessionsMu.Lock()
		if s.sessionsGeneration == generation && !clientGone(err) {
			s.sessionsCache[key] = &sessionsCacheEntry{query: query, sort: sort, dir: dir, page: page, pageSize: pageSize, result: result, err: err, fetched: time.Now(), hasResult: true, lastUse: time.Now()}
		} else {
			delete(s.sessionsCache, key)
		}
		ready := computing.ready
		s.sessionsMu.Unlock()
		close(ready)
		return result, err
	}
}

func (s *Server) startSessionsRefreshLocked(key string, entry *sessionsCacheEntry) {
	if !s.tryStartBackground() {
		return
	}
	s.sessionsRefreshes.Add(1)
	generation := s.sessionsGeneration
	query := entry.query
	query.Now = time.Now().UTC()
	sort := entry.sort
	dir := entry.dir
	page := entry.page
	pageSize := entry.pageSize
	ready := make(chan struct{})
	entry.ready = ready
	go func() {
		defer s.bgWG.Done()
		ctx, cancel := context.WithTimeout(s.backgroundContext(), sharedComputeTimeout)
		defer cancel()
		result, err := s.store.SessionsPage(ctx, query, sort, dir, page, pageSize)

		s.sessionsMu.Lock()
		if s.sessionsGeneration == generation && !clientGone(err) {
			s.sessionsCache[key] = &sessionsCacheEntry{query: query, sort: sort, dir: dir, page: page, pageSize: pageSize, result: result, err: err, fetched: time.Now(), hasResult: true, lastUse: time.Now()}
		}
		entry.ready = nil
		s.sessionsMu.Unlock()
		close(ready)
	}()
}

func (s *Server) evictSessionsEntriesLocked() {
	for len(s.sessionsCache) > sessionsCacheMaxEntries {
		victim := ""
		var oldest time.Time
		for key, entry := range s.sessionsCache {
			if entry.ready != nil {
				continue
			}
			if victim == "" || entry.lastUse.Before(oldest) {
				victim, oldest = key, entry.lastUse
			}
		}
		if victim == "" {
			return
		}
		delete(s.sessionsCache, victim)
	}
}

func (s *Server) invalidateSessionsCache() {
	now := time.Now()
	s.sessionsMu.Lock()
	s.sessionsGeneration++
	for _, entry := range s.sessionsCache {
		if entry.ready != nil {
			continue
		}
		due := now
		if windowEnd := entry.fetched.Add(sessionsMinRefreshInterval); windowEnd.After(now) {
			due = windowEnd
		}
		entry.refreshDue = due
	}
	s.sessionsMu.Unlock()
}

func (s *Server) cachedInsights(ctx context.Context, query database.AnalyticsQuery) (database.Insights, error) {
	key := analyticsCacheKey(query)
	for {
		s.insightsMu.Lock()
		if entry, ok := s.insightsCache[key]; ok && entry.ready != nil && !entry.hasResult {
			s.insightsMu.Unlock()
			select {
			case <-entry.ready:
				continue
			case <-ctx.Done():
				return database.Insights{}, ctx.Err()
			}
		}
		if entry, ok := s.insightsCache[key]; ok && entry.hasResult {
			stale := time.Since(entry.fetched) >= insightsCacheTTL ||
				(!entry.refreshDue.IsZero() && !time.Now().Before(entry.refreshDue))
			if stale && entry.ready == nil {
				s.startInsightsRefreshLocked(key, entry)
			}
			entry.lastUse = time.Now()
			result, err := entry.result, entry.err
			s.insightsMu.Unlock()
			return result, err
		}
		if s.insightsCache == nil {
			s.insightsCache = make(map[string]*insightsCacheEntry)
		}
		computing := &insightsCacheEntry{query: query, ready: make(chan struct{})}
		s.insightsCache[key] = computing
		s.evictInsightsEntriesLocked()
		generation := s.insightsGeneration
		s.insightsMu.Unlock()

		computeCtx, cancel := detachedComputeContext(ctx)
		result, err := s.store.Insights(computeCtx, query)
		cancel()

		s.insightsMu.Lock()
		if s.insightsGeneration == generation && !clientGone(err) {
			s.insightsCache[key] = &insightsCacheEntry{query: query, result: result, err: err, fetched: time.Now(), hasResult: true, lastUse: time.Now()}
		} else {
			delete(s.insightsCache, key)
		}
		ready := computing.ready
		s.insightsMu.Unlock()
		close(ready)
		return result, err
	}
}

func (s *Server) startInsightsRefreshLocked(key string, entry *insightsCacheEntry) {
	if !s.tryStartBackground() {
		return
	}
	s.insightsRefreshes.Add(1)
	generation := s.insightsGeneration
	query := entry.query
	query.Now = time.Now().UTC()
	ready := make(chan struct{})
	entry.ready = ready
	go func() {
		defer s.bgWG.Done()
		ctx, cancel := context.WithTimeout(s.backgroundContext(), sharedComputeTimeout)
		defer cancel()
		result, err := s.store.Insights(ctx, query)

		s.insightsMu.Lock()
		if s.insightsGeneration == generation && !clientGone(err) {
			s.insightsCache[key] = &insightsCacheEntry{query: query, result: result, err: err, fetched: time.Now(), hasResult: true, lastUse: time.Now()}
		}
		entry.ready = nil
		s.insightsMu.Unlock()
		close(ready)
	}()
}

func (s *Server) evictInsightsEntriesLocked() {
	for len(s.insightsCache) > insightsCacheMaxEntries {
		victim := ""
		var oldest time.Time
		for key, entry := range s.insightsCache {
			if entry.ready != nil {
				continue
			}
			if victim == "" || entry.lastUse.Before(oldest) {
				victim, oldest = key, entry.lastUse
			}
		}
		if victim == "" {
			return
		}
		delete(s.insightsCache, victim)
	}
}

func (s *Server) invalidateInsightsCache() {
	now := time.Now()
	s.insightsMu.Lock()
	s.insightsGeneration++
	for _, entry := range s.insightsCache {
		if entry.ready != nil {
			continue
		}
		due := now
		if windowEnd := entry.fetched.Add(insightsMinRefreshInterval); windowEnd.After(now) {
			due = windowEnd
		}
		entry.refreshDue = due
	}
	s.insightsMu.Unlock()
}

func (s *Server) timelineAPI(w http.ResponseWriter, r *http.Request) {
	result, err := s.cachedAnalytics(r.Context(), analyticsQueryFromRequest(r))
	if err != nil {
		if clientGone(err) {
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"filter":     result.Filter,
		"start_date": result.StartDate,
		"end_date":   result.EndDate,
		"bucket":     result.Bucket,
		"points":     result.Points,
	})
}

func (s *Server) analyticsExport(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	result, err := s.cachedAnalytics(r.Context(), analyticsQueryFromRequest(r))
	s.metrics.observeDB(started)
	if err != nil {
		if clientGone(err) {
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if r.URL.Query().Get("format") == "csv" {
		writeAnalyticsCSV(w, result)
		return
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	payload = append(payload, '\n')
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="tokemon-analytics-`+result.Filter.Period+`.json"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

// writeAnalyticsCSV streams the current breakdown as RFC 4180 CSV, honoring
// the active dimension and filters. Names are raw identifiers except the
// empty bucket, which gets the dimension-aware unknown label matching the
// analytics page (the JSON export keeps the raw sentinel). Share is the
// rendered percentage. No invented pricing or derived fields.
func writeAnalyticsCSV(w http.ResponseWriter, result database.Analytics) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="tokemon-analytics-`+result.Filter.Dimension+`-`+result.Filter.Period+`.csv"`)
	w.WriteHeader(http.StatusOK)
	writer := csv.NewWriter(w)
	defer writer.Flush()
	if err := writer.Write([]string{"dimension", "period", "name", "tokens", "events", "sessions", "share_percent"}); err != nil {
		return
	}
	for _, row := range result.Breakdown {
		name := row.Name
		if isUnknownBucketName(name) {
			name = analyticsUnknownLabel(result.Filter.Dimension)
		}
		if err := writer.Write([]string{
			csvFormulaSafe(result.Filter.Dimension),
			csvFormulaSafe(result.Filter.Period),
			csvFormulaSafe(name),
			strconv.FormatInt(row.Tokens, 10),
			strconv.FormatInt(row.Events, 10),
			strconv.FormatInt(row.Sessions, 10),
			strconv.FormatFloat(row.Share*100, 'f', 1, 64),
		}); err != nil {
			return
		}
	}
}

func quotaNoticeMessage(code string) string {
	switch code {
	case "1":
		return "Quota saved."
	case "override":
		return "Quota override saved in SQLite."
	case "deleted":
		return "Quota deleted."
	case "reset":
		return "UI override removed; YAML config quota restored."
	default:
		return ""
	}
}

func settingsQuotaScopeLabel(provider, model string) string {
	switch {
	case provider != "" && model != "":
		return provider + " · " + model
	case provider != "":
		return "Provider: " + provider
	case model != "":
		return "Model: " + model
	default:
		return "All usage (catch-all)"
	}
}

func settingsQuotaRows(configured []database.ConfiguredQuota, statuses []database.QuotaStatus) []settingsQuotaRow {
	views := quotaViews(statuses)
	viewByKey := make(map[string]quotaView, len(views))
	for _, v := range views {
		viewByKey[quota.Key(v.Provider, v.Model)] = v
	}
	rows := make([]settingsQuotaRow, 0, len(configured))
	for _, item := range configured {
		row := settingsQuotaRow{
			Provider:      item.Provider,
			Model:         item.Model,
			Label:         item.Label,
			DisplayLabel:  item.Label,
			ScopeLabel:    settingsQuotaScopeLabel(item.Provider, item.Model),
			ResetDay:      item.ResetDay,
			Source:        item.Source,
			OverridesYAML: item.OverridesYAML,
		}
		if row.DisplayLabel == "" {
			row.DisplayLabel = settingsQuotaScopeLabel(item.Provider, item.Model)
		}
		if item.Budget.TokensPerMonth != nil {
			row.TokensValue = strconv.FormatInt(*item.Budget.TokensPerMonth, 10)
		}
		if item.Budget.CostPerMonth != nil {
			row.CostValue = strconv.FormatFloat(*item.Budget.CostPerMonth, 'f', -1, 64)
		}
		switch {
		case item.Source == database.QuotaSourceYAML:
			row.SourceBadge = "YAML config"
			row.SaveLabel = "Override in UI"
		case item.OverridesYAML:
			row.SourceBadge = "UI override"
			row.CanDelete = true
			row.DeleteLabel = "Reset to YAML"
			row.SaveLabel = "Save override"
		default:
			row.SourceBadge = "UI (SQLite)"
			row.CanDelete = true
			row.DeleteLabel = "Delete"
			row.SaveLabel = "Save quota"
		}
		if v, ok := viewByKey[quota.Key(item.Provider, item.Model)]; ok {
			row.HasStatus = true
			row.StatusFigures = v.UsedLabel + " / " + v.BudgetLabel
			row.StatusDetail = v.Detail
			row.StatusPercent = v.Percent
			row.StatusFill = v.Fill
			row.StatusOver = v.Over
		}
		rows = append(rows, row)
	}
	return rows
}

func backupAgeLabel(age time.Duration) string {
	if age < 0 {
		age = 0
	}
	if age >= 24*time.Hour {
		days := int(age / (24 * time.Hour))
		hours := int((age % (24 * time.Hour)) / time.Hour)
		return fmt.Sprintf("%dd %dh ago", days, hours)
	}
	return durationLabel(age.Milliseconds()) + " ago"
}

func (s *Server) settingsBackupStatus() settingsBackupInfo {
	schedule := "Disabled"
	dbPath := strings.TrimSpace(s.config.DatabasePath)
	fileBacked := dbPath != "" && !strings.HasPrefix(dbPath, ":") && !strings.HasPrefix(dbPath, "file:")
	switch {
	case !fileBacked && strings.TrimSpace(s.config.BackupDir) == "":
		schedule = "Not configured"
	case s.config.BackupInterval > 0:
		retention := s.config.BackupRetention
		if retention < 1 {
			retention = 10
		}
		schedule = fmt.Sprintf("Every %s (retain %d)", s.config.BackupInterval.String(), retention)
	}
	info, err := database.LatestBackupInfo(s.config.DatabasePath, s.config.BackupDir)
	if err != nil {
		return settingsBackupInfo{
			StatusLabel: "Unavailable",
			Schedule:    schedule,
			Error:       true,
		}
	}
	if !info.Exists {
		return settingsBackupInfo{
			StatusLabel: "No backups recorded yet",
			Schedule:    schedule,
		}
	}
	return settingsBackupInfo{
		StatusLabel: "Latest backup",
		AgeLabel:    backupAgeLabel(info.Age),
		TimestampUI: analyticsShortTimeAt(info.Timestamp.UTC().Format(time.RFC3339), s.store.Location(), time.Now()),
		Count:       info.Count,
		Schedule:    schedule,
		Exists:      true,
	}
}

func (s *Server) buildSettingsPage(ctx context.Context, saved bool, quotaNotice, errMsg string) (settingsPage, error) {
	started := time.Now()
	result, _, err := s.cachedOverview(ctx)
	s.metrics.observeDB(started)
	if err != nil {
		return settingsPage{}, err
	}
	started = time.Now()
	aliases, err := s.store.DisplayAliases(ctx)
	s.metrics.observeDB(started)
	if err != nil {
		return settingsPage{}, err
	}
	started = time.Now()
	configuredQuotas, err := s.store.Quotas(ctx)
	s.metrics.observeDB(started)
	if err != nil {
		return settingsPage{}, err
	}
	statuses := s.store.QuotaStatus(ctx, time.Now())
	modelAliases, machineAliases := aliasMaps(aliases)
	authMode, tokenSource := s.authSummary()
	return settingsPage{
		PageChrome:  s.pageChrome("Tokemon · Settings", "settings", false),
		Models:      modelAliasRows(result.ByModel, modelAliases, s.config.EnableTokedex),
		Machines:    machineAliasRows(result.ByMachine, machineAliases),
		Quotas:      settingsQuotaRows(configuredQuotas, statuses),
		Backup:      s.settingsBackupStatus(),
		Saved:       saved,
		QuotaNotice: quotaNotice,
		Error:       errMsg,
		AuthMode:    authMode,
		TokenSource: tokenSource,
	}, nil
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	page, err := s.buildSettingsPage(
		r.Context(),
		r.URL.Query().Get("saved") == "1",
		quotaNoticeMessage(r.URL.Query().Get("quota_saved")),
		"",
	)
	if err != nil {
		if clientGone(err) {
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.template.ExecuteTemplate(w, "settings", page); err != nil {
		return
	}
}

func (s *Server) renderSettingsError(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	page, err := s.buildSettingsPage(r.Context(), false, "", errMsg)
	if err != nil {
		writeAccessibleError(w, status, errMsg)
		return
	}
	var buf bytes.Buffer
	if err := s.template.ExecuteTemplate(&buf, "settings", page); err != nil {
		writeAccessibleError(w, status, errMsg)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) saveQuota(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		writeAccessibleError(w, http.StatusBadRequest, "Invalid quota form.")
		return
	}
	if !s.validSettingsOrigin(r) || !s.validCSRFToken(r.Form.Get("csrf_token")) {
		writeAccessibleError(w, http.StatusForbidden, "The settings form could not be verified. Reload the page and try again.")
		return
	}

	action := strings.ToLower(strings.TrimSpace(r.Form.Get("action")))
	provider := strings.TrimSpace(r.Form.Get("provider"))
	model := strings.TrimSpace(r.Form.Get("model"))
	if action == "delete" {
		overridesYAML := r.Form.Get("overrides_yaml") == "1"
		started := time.Now()
		err := s.store.DeleteQuota(r.Context(), provider, model)
		s.metrics.observeDB(started)
		if err != nil {
			s.renderSettingsError(w, r, http.StatusBadRequest, "Could not delete quota: "+err.Error())
			return
		}
		s.dropOverviewHTMLCache()
		s.invalidateAnalyticsCache()
		s.resetAlertStateForQuota(provider, model)
		s.triggerQuotaAlerts(false)
		code := "deleted"
		if overridesYAML {
			code = "reset"
		}
		http.Redirect(w, r, "/settings?quota_saved="+code, http.StatusSeeOther)
		return
	}

	resetDay := 1
	if rawDay := strings.TrimSpace(r.Form.Get("reset_day")); rawDay != "" {
		parsed, err := strconv.Atoi(rawDay)
		if err != nil {
			s.renderSettingsError(w, r, http.StatusBadRequest, "Quota reset day must be a whole number between 1 and 31.")
			return
		}
		resetDay = parsed
	}
	var tokensPtr *int64
	if rawTokens := strings.TrimSpace(r.Form.Get("tokens_per_month")); rawTokens != "" {
		parsed, err := strconv.ParseInt(rawTokens, 10, 64)
		if err != nil {
			s.renderSettingsError(w, r, http.StatusBadRequest, "Monthly token budget must be a whole number.")
			return
		}
		tokensPtr = &parsed
	}
	var costPtr *float64
	if rawCost := strings.TrimSpace(r.Form.Get("cost_per_month")); rawCost != "" {
		parsed, err := strconv.ParseFloat(rawCost, 64)
		if err != nil {
			s.renderSettingsError(w, r, http.StatusBadRequest, "Monthly cost budget must be a valid decimal USD amount.")
			return
		}
		costPtr = &parsed
	}

	candidate := quota.Quota{
		Provider: provider,
		Model:    model,
		Label:    r.Form.Get("label"),
		ResetDay: resetDay,
		Budget: quota.Budget{
			TokensPerMonth: tokensPtr,
			CostPerMonth:   costPtr,
		},
	}

	existing, err := s.store.Quotas(r.Context())
	isYAMLOverride := false
	if err == nil {
		key := quota.Key(candidate.Provider, candidate.Model)
		for _, item := range existing {
			if quota.Key(item.Provider, item.Model) == key && (item.Source == database.QuotaSourceYAML || item.OverridesYAML) {
				isYAMLOverride = true
				break
			}
		}
	}

	started := time.Now()
	if err := s.store.UpsertQuota(r.Context(), candidate); err != nil {
		s.metrics.observeDB(started)
		s.renderSettingsError(w, r, http.StatusBadRequest, "Could not save quota: "+err.Error())
		return
	}
	s.metrics.observeDB(started)
	s.dropOverviewHTMLCache()
	s.invalidateAnalyticsCache()
	s.resetAlertStateForQuota(candidate.Provider, candidate.Model)
	s.triggerQuotaAlerts(false)
	code := "1"
	if isYAMLOverride {
		code = "override"
	}
	http.Redirect(w, r, "/settings?quota_saved="+code, http.StatusSeeOther)
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		writeAccessibleError(w, http.StatusBadRequest, "Invalid settings form.")
		return
	}
	if !s.validSettingsOrigin(r) || !s.validCSRFToken(r.Form.Get("csrf_token")) {
		writeAccessibleError(w, http.StatusForbidden, "The settings form could not be verified. Reload the page and try again.")
		return
	}
	started := time.Now()
	if err := saveAliasGroup(r.Context(), s.store, database.AliasKindModel, r.Form["model_identity"], r.Form["model_alias"]); err != nil {
		s.metrics.observeDB(started)
		writeAccessibleError(w, http.StatusBadRequest, "Could not save model aliases. Check each alias and try again.")
		return
	}
	s.metrics.observeDB(started)
	started = time.Now()
	if err := saveAliasGroup(r.Context(), s.store, database.AliasKindMachine, r.Form["machine_identity"], r.Form["machine_alias"]); err != nil {
		s.metrics.observeDB(started)
		writeAccessibleError(w, http.StatusBadRequest, "Could not save machine aliases. Check each alias and try again.")
		return
	}
	s.metrics.observeDB(started)
	// Alias edits change the rendered dashboard page; drop the cached page so
	// the next view renders with the new names instead of waiting out the TTL.
	s.dropOverviewHTMLCache()
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}

func saveAliasGroup(ctx context.Context, store *database.Store, kind string, identities, aliases []string) error {
	if len(identities) != len(aliases) {
		return fmt.Errorf("invalid %s alias form", kind)
	}
	for index, identity := range identities {
		if err := store.SetDisplayAlias(ctx, kind, identity, aliases[index]); err != nil {
			return err
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAccessibleError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<!doctype html><html lang=\"en\"><head><meta charset=\"utf-8\"><title>Tokemon error</title></head><body><main><h1>Settings error</h1><div role=\"alert\" tabindex=\"-1\">%s</div><p><a href=\"/settings\">Return to settings</a></p></main></body></html>", template.HTMLEscapeString(message))
}

// activityInitialTab picks the date the heatmap's roving tabindex starts on:
// the most recent active day, so keyboard users land at the newest end of the
// year and arrow across or backwards from there instead of stepping through
// one stop per active day. With no activity at all it falls back to the most
// recent non-future day (today), so the grid always loads with exactly one
// tab stop and stays keyboard reachable.
func activityInitialTab(activity database.ActivityHeatmap) string {
	if date := activityLastDay(activity, func(day database.ActivityDay) bool {
		return day.Level > 0 || day.UnknownTokens > 0
	}); date != "" {
		return date
	}
	return activityLastDay(activity, func(database.ActivityDay) bool { return true })
}

// activityLastDay returns the newest non-future day matching match, or "".
func activityLastDay(activity database.ActivityHeatmap, match func(database.ActivityDay) bool) string {
	for week := len(activity.Weeks) - 1; week >= 0; week-- {
		days := activity.Weeks[week].Days
		for index := len(days) - 1; index >= 0; index-- {
			if !days[index].Future && match(days[index]) {
				return days[index].Date
			}
		}
	}
	return ""
}

// activityTabStop renders one heatmap cell's roving-tabindex value: the
// initial tab stop is the only cell in the tab order, and every other
// non-future day is reachable through the arrow keys (tabindex="-1").
func activityTabStop(day database.ActivityDay, initial string) string {
	if day.Date == initial {
		return "0"
	}
	return "-1"
}

func activityTooltip(day database.ActivityDay) string {
	if day.Future {
		return ""
	}
	if day.Events == 0 {
		return day.Label + " · No token usage"
	}
	var builder strings.Builder
	builder.WriteString(day.Label)
	builder.WriteByte('\n')
	eventWord := "events"
	if day.Events == 1 {
		eventWord = "event"
	}
	if day.UnknownTokens >= day.Events {
		fmt.Fprintf(&builder, "Token total unavailable · %s %s", commas(day.Events), eventWord)
	} else {
		tokenLabel := commas(day.Tokens)
		fmt.Fprintf(&builder, "%s tokens · %s %s", tokenLabel, commas(day.Events), eventWord)
	}
	if day.UnknownTokens > 0 {
		builder.WriteString("\nSome token totals unavailable")
	}
	appendActivityBreakdown(&builder, "BY MODEL", day.ByModel)
	appendActivityBreakdown(&builder, "BY PROVIDER", day.ByProvider)
	return builder.String()
}

func appendActivityBreakdown(builder *strings.Builder, heading string, values []database.TokenBreakdown) {
	if len(values) == 0 {
		return
	}
	builder.WriteByte('\n')
	builder.WriteString(heading)
	for _, value := range values {
		if value.UnknownTokens > 0 && value.Tokens == 0 {
			fmt.Fprintf(builder, "\n  %s · token total unavailable", value.Name)
			continue
		}
		tokenLabel := commas(value.Tokens)
		fmt.Fprintf(builder, "\n  %s · %s tokens", value.Name, tokenLabel)
	}
}

func commas(value int64) string {
	text := strconv.FormatInt(value, 10)
	start := 0
	if strings.HasPrefix(text, "-") {
		start = 1
	}
	first := (len(text) - start) % 3
	if first == 0 {
		first = 3
	}
	var builder strings.Builder
	builder.WriteString(text[:start+first])
	for index := start + first; index < len(text); index += 3 {
		builder.WriteByte(',')
		builder.WriteString(text[index : index+3])
	}
	return builder.String()
}

func money(value float64) string {
	cents := int64(math.Round(value * 100))
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s$%s.%02d", sign, commas(cents/100), cents%100)
}

// durationLabel keeps sub-minute precision and switches to clock units for
// long sessions, where fractional seconds are noise.
func durationLabel(ms int64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
	seconds := (ms + 500) / 1000
	if seconds < 3600 {
		return fmt.Sprintf("%dm %02ds", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%dh %02dm", seconds/3600, seconds%3600/60)
}

func commasPtr(value *int64) string {
	if value == nil {
		return ""
	}
	return commas(*value)
}

func compactPtr(value *int64) string {
	if value == nil {
		return ""
	}
	abs := float64(*value)
	if abs < 0 {
		abs = -abs
	}
	for _, unit := range []struct {
		threshold float64
		suffix    string
	}{
		{1_000_000_000_000, "T"},
		{1_000_000_000, "B"},
		{1_000_000, "M"},
		{1_000, "K"},
	} {
		if abs >= unit.threshold {
			return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(*value)/unit.threshold), ".0") + unit.suffix
		}
	}
	return strconv.FormatInt(*value, 10)
}

func compact(value int64) string {
	return compactPtr(&value)
}

func cacheUncached(value database.CacheSummary) int64 {
	if value.EligibleTokens <= value.CachedTokens {
		return 0
	}
	return value.EligibleTokens - value.CachedTokens
}

func twoDigits(value int) string {
	if value < 0 {
		value = 0
	}
	if value > 99 {
		value = 99
	}
	return strconv.FormatInt(int64(value/10), 10) + strconv.FormatInt(int64(value%10), 10)
}
