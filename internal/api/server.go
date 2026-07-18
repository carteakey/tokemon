package api

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/evolution"
	"github.com/tokemon/tokemon/internal/usage"
	"github.com/tokemon/tokemon/web"
)

type Server struct {
	store       *database.Store
	ingestToken string
	template    *template.Template
	static      http.Handler
}

type dashboardPage struct {
	database.Overview
	ModelAliases   map[string]string
	MachineAliases map[string]string
}

type analyticsPageData struct {
	database.Analytics
	ModelAliases   map[string]string
	MachineAliases map[string]string
	ExportURL      string
	PeakPoint      database.AnalyticsPoint
	AverageThread  int64
}

type aliasRow struct {
	Identity string
	Alias    string
	Default  string
}

type settingsPage struct {
	Models   []aliasRow
	Machines []aliasRow
	Saved    bool
	Error    string
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
	case "opencode":
		return glyphFromRows("harness", "opencode", 2, "ddddd", "o...d", "d.ood", "d...o", "ddddd")
	case "copilot-cli", "github-copilot":
		return glyphFromRows("harness", "copilot", 3, "d...d", ".ddd.", "ddddd", ".ddd.", "d...d")
	case "antigravity", "gemini":
		return glyphFromRows("harness", "gemini", 0, "..o..", ".odo.", "ododo", ".odo.", "..o..")
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
	switch tool {
	case "claude-code":
		return "Claude Code"
	case "codex":
		return "Codex"
	case "opencode":
		return "OpenCode"
	case "copilot-cli", "github-copilot":
		return "GitHub Copilot CLI"
	case "antigravity":
		return "Antigravity"
	case "generic-jsonl":
		return "Generic JSONL"
	default:
		return tool
	}
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
		for index := range parts {
			parts[index] = titleWord(parts[index])
		}
		return strings.Join(parts, " ")
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
			return strings.Join(parts[index:], " ")
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

func analyticsShortTime(raw string) string {
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return raw
	}
	return parsed.UTC().Format("Jan 2, 15:04")
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
		Now:       time.Now().UTC(),
	}
}

func analyticsExportURL(query database.AnalyticsQuery) string {
	values := url.Values{}
	values.Set("period", query.Period)
	values.Set("dimension", query.Dimension)
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
	return "/api/v1/analytics/export?" + values.Encode()
}

func analyticsViewURL(query database.AnalyticsQuery, dimension string) string {
	query.Dimension = dimension
	values := url.Values{}
	values.Set("period", query.Period)
	values.Set("dimension", query.Dimension)
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
	return "/analytics?" + values.Encode()
}

func analyticsPeriodURL(query database.AnalyticsQuery, period string) string {
	query.Period = period
	return analyticsViewURL(query, query.Dimension)
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

func modelAliasRows(values []database.ModelTotal, aliases map[string]string) []aliasRow {
	rows := make([]aliasRow, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		seen[value.Model] = struct{}{}
		rows = append(rows, aliasRow{Identity: value.Model, Alias: aliases[value.Model], Default: compactModelName(value.Model)})
	}
	for identity, alias := range aliases {
		if _, ok := seen[identity]; !ok {
			rows = append(rows, aliasRow{Identity: identity, Alias: alias, Default: compactModelName(identity)})
		}
	}
	sort.SliceStable(rows, func(left, right int) bool { return rows[left].Identity < rows[right].Identity })
	return rows
}

func machineAliasRows(values []database.MachineTotal, aliases map[string]string) []aliasRow {
	rows := make([]aliasRow, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		seen[value.Machine] = struct{}{}
		rows = append(rows, aliasRow{Identity: value.Machine, Alias: aliases[value.Machine], Default: compactMachineName(value.Machine)})
	}
	for identity, alias := range aliases {
		if _, ok := seen[identity]; !ok {
			rows = append(rows, aliasRow{Identity: identity, Alias: alias, Default: compactMachineName(identity)})
		}
	}
	sort.SliceStable(rows, func(left, right int) bool { return rows[left].Identity < rows[right].Identity })
	return rows
}

func limitDashboardRows[T any](values []T) []T {
	if len(values) <= dashboardUsageRowsLimit {
		return values
	}
	return values[:dashboardUsageRowsLimit]
}

func New(store *database.Store, ingestToken string) (*Server, error) {
	page, err := template.New("dashboard").Funcs(template.FuncMap{
		"commas":          commas,
		"commasPtr":       commasPtr,
		"activityTooltip": activityTooltip,
		"mul":             func(left, right float64) float64 { return left * right },
		"percent":         func(value float64) float64 { return value * 100 },
		"money":           func(value float64) string { return fmt.Sprintf("$%.2f", value) },
		"compact":         compact,
		"cacheUncached":   cacheUncached,
		"compactPtr":      compactPtr,
		"share": func(part, total int64) string {
			if total == 0 {
				return "0.0%"
			}
			return fmt.Sprintf("%.1f%%", float64(part)*100/float64(total))
		},
		"topProjects":          topProjects,
		"topModels":            topModels,
		"topTools":             topTools,
		"harnessName":          harnessName,
		"modelDisplayName":     modelDisplayName,
		"machineDisplayName":   machineDisplayName,
		"analyticsBar":         analyticsBarPercent,
		"analyticsPart":        analyticsPartPercent,
		"analyticsTime":        analyticsShortTime,
		"analyticsURL":         analyticsViewURL,
		"analyticsPeriodURL":   analyticsPeriodURL,
		"analyticsChange":      analyticsChangeLabel,
		"analyticsChangeClass": analyticsChangeClass,
		"glyphCell":            glyphCellClass,
		"projectGlyph":         glyphForProject,
		"harnessGlyph":         glyphForHarness,
		"modelGlyph":           glyphForModel,
		"machineGlyph":         glyphForMachine,
		"assetPath":            func(stage int) string { return "/static/tokemon/stage-" + twoDigits(stage) + ".png" },
	}).Parse(dashboardTemplate)
	if err != nil {
		return nil, err
	}
	if _, err := page.Parse(settingsTemplate); err != nil {
		return nil, err
	}
	if _, err := page.Parse(analyticsTemplate); err != nil {
		return nil, err
	}
	staticFiles, err := fs.Sub(web.StaticFS, "static")
	if err != nil {
		return nil, err
	}
	return &Server{store: store, ingestToken: ingestToken, template: page, static: http.FileServer(http.FS(staticFiles))}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.Handle("GET /static/", http.StripPrefix("/static/", s.static))
	mux.HandleFunc("GET /", s.dashboard)
	mux.HandleFunc("GET /analytics", s.analyticsPage)
	mux.HandleFunc("GET /data", s.analyticsPage)
	mux.HandleFunc("GET /settings", s.settings)
	mux.HandleFunc("POST /settings/aliases", s.saveSettings)
	mux.HandleFunc("POST /api/v1/events/batch", s.ingest)
	mux.HandleFunc("GET /api/v1/evolution", s.evolution)
	mux.HandleFunc("GET /api/v1/analytics/overview", s.overview)
	mux.HandleFunc("GET /api/v1/analytics", s.analyticsAPI)
	mux.HandleFunc("GET /api/v1/analytics/export", s.analyticsExport)
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type batchRequest struct {
	Events []usage.Event `json:"events"`
}

func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	if s.ingestToken != "" && !validToken(r, s.ingestToken) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid ingest token"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	var reader io.Reader = r.Body
	var compressed *gzip.Reader
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		var err error
		compressed, err = gzip.NewReader(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid gzip body"})
			return
		}
		defer compressed.Close()
		reader = compressed
	}
	var request batchRequest
	if err := json.NewDecoder(reader).Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if len(request.Events) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "events must contain at least one event"})
		return
	}
	result, err := s.store.Ingest(r.Context(), request.Events)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func validToken(r *http.Request, expected string) bool {
	provided := r.Header.Get("X-Tokemon-Ingest-Token")
	if provided == "" {
		provided = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	return provided == expected
}

func (s *Server) evolution(w http.ResponseWriter, r *http.Request) {
	result, err := s.store.Evolution(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	composition, err := s.store.TokenComposition(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, struct {
		evolution.Snapshot
		Composition database.TokenComposition `json:"composition"`
	}{Snapshot: result, Composition: composition})
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	result, err := s.store.Overview(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	result, err := s.store.Overview(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	aliases, err := s.store.DisplayAliases(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	modelAliases, machineAliases := aliasMaps(aliases)
	page := dashboardPage{Overview: result, ModelAliases: modelAliases, MachineAliases: machineAliases}
	if err := s.template.ExecuteTemplate(w, "dashboard", page); err != nil {
		return
	}
}

func (s *Server) analyticsPage(w http.ResponseWriter, r *http.Request) {
	query := analyticsQueryFromRequest(r)
	result, err := s.store.Analytics(r.Context(), query)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	aliases, err := s.store.DisplayAliases(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	modelAliases, machineAliases := aliasMaps(aliases)
	page := analyticsPageData{
		Analytics:      result,
		ModelAliases:   modelAliases,
		MachineAliases: machineAliases,
		ExportURL:      analyticsExportURL(result.Filter),
		PeakPoint:      analyticsPeakPoint(result.Points),
		AverageThread:  analyticsAverageThread(result.Summary),
	}
	if err := s.template.ExecuteTemplate(w, "analytics", page); err != nil {
		return
	}
}

func (s *Server) analyticsAPI(w http.ResponseWriter, r *http.Request) {
	result, err := s.store.Analytics(r.Context(), analyticsQueryFromRequest(r))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) analyticsExport(w http.ResponseWriter, r *http.Request) {
	result, err := s.store.Analytics(r.Context(), analyticsQueryFromRequest(r))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	result, err := s.store.Overview(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	aliases, err := s.store.DisplayAliases(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	modelAliases, machineAliases := aliasMaps(aliases)
	page := settingsPage{
		Models:   modelAliasRows(result.ByModel, modelAliases),
		Machines: machineAliasRows(result.ByMachine, machineAliases),
		Saved:    r.URL.Query().Get("saved") == "1",
	}
	if err := s.template.ExecuteTemplate(w, "settings", page); err != nil {
		return
	}
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid settings form", http.StatusBadRequest)
		return
	}
	if err := saveAliasGroup(r.Context(), s.store, database.AliasKindModel, r.Form["model_identity"], r.Form["model_alias"]); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := saveAliasGroup(r.Context(), s.store, database.AliasKindMachine, r.Form["machine_identity"], r.Form["machine_alias"]); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
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

const dashboardTemplate = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta http-equiv="refresh" content="60">
  <meta name="theme-color" content="#10110f">
  <link rel="icon" type="image/png" href="/static/tokemon/token-dex.png">
  <title>Tokemon · Overview</title>
  <style>
    @font-face {
      font-family: "Pixelify Sans";
      font-style: normal;
      font-weight: 400 700;
      font-display: swap;
      src: url("/static/tokemon/fonts/pixelify-sans-latin.woff2") format("woff2");
    }
    :root {
      color-scheme: dark;
      --font-display: "Pixelify Sans", ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      --bg: #10110f;
      --surface: #171916;
      --surface-raised: #1d201b;
      --surface-soft: #22261f;
      --text: #f0ede5;
      --muted: #a2a69b;
      --faint: #6f766b;
      --line: #30352d;
      --line-bright: #485044;
      --accent: #9bbba0;
      --accent-dim: #607864;
      --warm: #d2a477;
      --danger: #bd7164;
      --font-data: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace;
    }
    * { box-sizing: border-box; }
    [hidden] { display: none !important; }
    body {
      margin: 0;
      background: var(--bg);
      color: var(--text);
      font: 14px/1.5 Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    }
    a { color: inherit; text-decoration: none; }
    button { font: inherit; }
    main { width: min(1408px, calc(100% - 40px)); margin: 20px auto; padding: 14px; border: 1px solid var(--line); border-radius: 12px; }
    .topbar { display: grid; align-items: center; grid-template-columns: 1fr auto 1fr; gap: 24px; padding: 0 10px 14px; border-bottom: 1px solid var(--line); }
    .brand { display: flex; align-items: center; gap: 12px; }
    .brand-mark { display: block; width: 34px; height: 34px; object-fit: contain; image-rendering: pixelated; filter: drop-shadow(0 4px 6px rgba(0, 0, 0, .22)); }
    .brand-name { font-family: var(--font-display); font-size: 16px; font-weight: 700; letter-spacing: .08em; }
    .nav { display: flex; align-items: center; grid-column: 2; gap: 18px; }
    .nav-link { padding: 7px 11px 6px; border-bottom: 2px solid transparent; color: var(--muted); font: 12px/1 var(--font-data); letter-spacing: .08em; text-transform: uppercase; }
    .nav-link.active { border-bottom-color: var(--accent); color: var(--accent); }
    .settings-link { display: grid; width: 28px; height: 28px; place-items: center; justify-self: end; border: 1px solid var(--line-bright); border-radius: 50%; color: var(--muted); font-size: 16px; line-height: 1; }
    .settings-link:hover, .settings-link:focus-visible { border-color: var(--accent); color: var(--accent); outline: none; }
    .hero { display: grid; grid-template-columns: minmax(280px, .82fr) minmax(0, 1.45fr); gap: 10px; padding: 10px 0; }
    .hero > .panel, .content-grid > .panel { min-width: 0; }
    .panel { border: 1px solid var(--line-bright); border-radius: 8px; background: var(--surface); box-shadow: inset 0 0 0 1px rgba(255, 255, 255, .012); }
    .creature-panel { position: relative; min-height: 360px; overflow: hidden; padding: 14px 16px; background: radial-gradient(circle at 50% 40%, rgba(155, 187, 160, .11), transparent 48%), var(--surface); }
    .creature-art-wrap { display: grid; min-height: 252px; place-items: center; padding: 2px 20px 0; }
    .creature-art { width: min(100%, 278px); max-height: 252px; object-fit: contain; image-rendering: pixelated; filter: drop-shadow(0 20px 24px rgba(0, 0, 0, .22)); transition: transform .35s ease, filter .35s ease; }
    .creature-art:hover { transform: translateY(-5px) scale(1.02); filter: drop-shadow(0 25px 30px rgba(0, 0, 0, .32)); }
    .creature-fallback { display: grid; width: 220px; height: 220px; place-items: center; border: 1px dashed var(--line-bright); border-radius: 50%; color: var(--accent); font: 700 30px ui-monospace, SFMono-Regular, Menlo, monospace; }
    .creature-fallback[hidden] { display: none; }
    .creature-caption { display: flex; align-items: center; flex-direction: column; gap: 6px; padding-top: 3px; text-align: center; }
    .form-name { margin-top: 2px; color: var(--text); font-family: var(--font-display); font-size: clamp(24px, 3vw, 34px); font-weight: 700; letter-spacing: .02em; }
    .stage-chip { padding: 5px 10px; border: 1px solid var(--warm); border-radius: 5px; color: var(--warm); font: 600 10px/1 var(--font-data); letter-spacing: .08em; white-space: nowrap; }
    .power-panel { display: flex; min-height: 360px; flex-direction: column; padding: 16px 18px; }
    .power-kicker { color: var(--accent); font: 700 12px/1 var(--font-data); letter-spacing: .08em; text-transform: uppercase; }
    .power-value { display: flex; align-items: center; width: 100%; min-width: 0; height: 1.14em; margin: 9px 0 0; overflow: hidden; column-gap: 3px; color: var(--text); font: 700 clamp(30px, 6vw, 90px)/1 var(--font-data); font-variant-numeric: tabular-nums; letter-spacing: 0; white-space: nowrap; }
    .odometer-static { white-space: nowrap; }
    .odometer-reel { position: relative; display: block; flex: 0 0 .82em; height: 1em; overflow: hidden; border: 1px solid var(--line-bright); border-radius: 4px; background: linear-gradient(180deg, #292c26 0 49%, #1d201b 50% 100%); box-shadow: inset 0 1px rgba(255,255,255,.035), inset 0 -8px 18px rgba(0,0,0,.16); line-height: 1; }
    .odometer-reel::after { position: absolute; z-index: 2; top: 50%; right: 0; left: 0; border-top: 1px solid rgba(8, 9, 8, .65); border-bottom: 1px solid rgba(255, 255, 255, .025); content: ""; pointer-events: none; }
    .odometer-separator { display: block; flex: 0 0 .2em; color: var(--muted); line-height: 1; text-align: center; }
    .odometer-strip { display: flex; flex-direction: column; transform: translateY(0); transition: transform 1.35s cubic-bezier(.2, .75, .2, 1); will-change: transform; }
    .odometer-digit { display: grid; flex: 0 0 1em; height: 1em; place-items: center; color: var(--text); line-height: 1; text-align: center; text-shadow: 0 2px 0 rgba(0,0,0,.32); }
    .odometer.is-ready .odometer-static, .mini-odometer.is-ready .odometer-static { display: none; }
    .power-composition { margin-top: 18px; }
    .composition-meter { display: flex; height: 30px; overflow: hidden; gap: 3px; border: 0; border-radius: 4px; background: transparent; }
    .composition-segment { display: block; min-width: 0; transition: width .45s ease; }
    .composition-segment.uncached { background: var(--accent-dim); }
    .composition-segment.cached { background: var(--accent); }
    .composition-segment.output { background: var(--warm); }
    .composition-primary { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 8px; margin-top: 9px; }
    .composition-item { display: flex; min-width: 0; align-items: start; flex-direction: column; justify-content: start; gap: 4px; padding: 8px 10px 7px; border: 1px solid var(--line); border-radius: 5px; background: var(--surface-raised); color: var(--muted); cursor: help; font: 700 10px/1.2 var(--font-data); letter-spacing: .06em; text-transform: uppercase; }
    .composition-item:hover, .composition-item:focus-visible { border-color: var(--accent); outline: 2px solid rgba(155, 187, 160, .22); outline-offset: 2px; }
    .composition-item.output { border-color: rgba(210, 164, 119, .58); background: rgba(210, 164, 119, .06); }
    .composition-item.output:hover, .composition-item.output:focus-visible { border-color: var(--warm); outline-color: rgba(210, 164, 119, .24); }
    .composition-item strong { color: var(--text); font-size: 20px; font-weight: 700; letter-spacing: 0; line-height: 1; }
    .composition-item.output strong { color: var(--warm); font-size: 22px; }
    .mini-odometer { display: flex; align-items: center; width: 100%; min-width: 0; height: 1.14em; margin-top: 2px; overflow: hidden; column-gap: 1px; color: var(--text); font: 700 20px/1 var(--font-data); font-variant-numeric: tabular-nums; letter-spacing: 0; white-space: nowrap; }
    .mini-odometer .odometer-reel { flex-basis: .82em; }
    .mini-odometer .odometer-separator { flex-basis: .18em; }
    .composition-item.output .odometer-digit, .composition-item.output .odometer-separator { color: var(--warm); }
    .composition-swatch { display: inline-block; width: 7px; height: 7px; margin: 0 5px 1px 0; border-radius: 1px; }
    .composition-swatch.output { background: var(--warm); }
    .composition-swatch.input { background: var(--accent-dim); }
    .progress-block { margin-top: auto; padding-top: 18px; }
    .progress-row { display: flex; align-items: center; justify-content: space-between; gap: 16px; color: var(--accent); font: 700 12px/1 var(--font-data); letter-spacing: .06em; text-transform: uppercase; }
    .progress-row strong { color: var(--text); font-weight: 600; }
    .progress-value { display: flex; align-items: baseline; justify-content: space-between; gap: 20px; margin-top: 7px; font: 700 20px/1 var(--font-data); }
    .progress-value strong:first-child { color: var(--warm); }
    .progress-value small { color: var(--text); font-size: 10px; letter-spacing: .08em; text-transform: uppercase; }
    .progress-value strong:last-child { color: var(--accent); }
    .progress { height: 22px; margin: 8px 0 0; overflow: hidden; border: 1px solid var(--line-bright); border-radius: 4px; background: #11130f; }
    .progress span { display: block; width: {{percent .Evolution.Progress}}%; height: 100%; border-radius: 2px; background: linear-gradient(90deg, var(--accent-dim), var(--accent)); box-shadow: 0 0 18px rgba(155, 187, 160, .2); }
    .stat-grid { display: grid; grid-template-columns: repeat(5, minmax(0, 1fr)); gap: 10px; padding: 0 0 10px; }
    .stat { display: grid; min-width: 0; min-height: 76px; grid-template-columns: 36px minmax(0, 1fr); align-items: center; gap: 11px; padding: 10px 13px; border: 1px solid var(--line-bright); border-radius: 7px; background: var(--surface); }
    .stat-icon { display: block; width: 36px; height: 36px; object-fit: contain; image-rendering: pixelated; }
    .stat-copy { min-width: 0; }
    .stat-label { color: var(--accent); font: 700 10px/1 var(--font-data); letter-spacing: .1em; text-transform: uppercase; }
    .stat-value { margin-top: 8px; overflow: hidden; color: var(--text); font: 700 20px/1.1 var(--font-data); text-overflow: ellipsis; white-space: nowrap; }
    .stat-value.muted { color: var(--muted); font-size: 13px; font-weight: 500; }
    .stat-detail { display: block; margin-top: 4px; overflow: hidden; color: var(--faint); font: 9px/1.2 var(--font-data); text-overflow: ellipsis; white-space: nowrap; }
    .activity-panel { margin-bottom: 10px; padding: 12px 16px; border-radius: 7px; }
    .activity-shell { display: flex; gap: 8px; margin-top: 7px; min-width: 0; }
    .activity-weekday-labels { display: grid; flex: 0 0 26px; grid-template-rows: 12px repeat(7, 12px); gap: 2px; color: var(--faint); font: 8px/12px var(--font-data); text-align: right; }
    .activity-weekday-labels span { height: 12px; }
    .activity-scroll { min-width: 0; flex: 1; overflow-x: auto; padding: 0 3px 7px 0; scrollbar-color: var(--line-bright) transparent; }
    .activity-grid { display: grid; width: 100%; min-width: 670px; grid-template-columns: repeat(53, minmax(9px, 1fr)); gap: 2px; }
    .activity-week { display: grid; min-width: 0; grid-template-rows: 12px repeat(7, 12px); gap: 2px; }
    .activity-month { overflow: visible; color: var(--faint); font: 8px/12px var(--font-data); white-space: nowrap; }
    .activity-cell { display: block; width: 100%; height: 12px; border: 1px solid #293127; border-radius: 0; background: #1a1f19; image-rendering: pixelated; }
    .activity-cell.level-1 { border-color: #36533b; background: #2a4430; }
    .activity-cell.level-2 { border-color: #4f724c; background: #416b46; }
    .activity-cell.level-3 { border-color: #779b5f; background: #6b8d56; }
    .activity-cell.level-4 { border-color: #aecb79; background: #9bb76d; }
    .activity-cell.unknown { border-color: var(--warm); }
    .activity-cell.future { border-color: transparent; background: transparent; box-shadow: none; }
    .activity-cell:not(.future) { cursor: pointer; }
    .activity-cell:not(.future):hover, .activity-cell:not(.future):focus-visible { position: relative; z-index: 1; outline: 2px solid var(--warm); outline-offset: 2px; }
    .activity-legend { display: flex; align-items: center; gap: 4px; color: var(--faint); font: 9px var(--font-data); }
    .activity-legend .activity-cell { width: 11px; height: 11px; }
    .activity-empty { margin-top: 4px; color: var(--faint); font-size: 12px; }
    .activity-tooltip { position: fixed; z-index: 20; width: min(320px, calc(100vw - 24px)); padding: 15px 16px 16px; border: 2px solid var(--warm); border-radius: 0; background: #12160f; box-shadow: 4px 4px 0 #090b08, inset 0 0 0 1px rgba(155, 187, 160, .16); color: var(--text); pointer-events: none; }
    .activity-tooltip[hidden] { display: none; }
    .activity-tooltip-date { color: var(--accent); font-family: var(--font-display); font-size: 15px; font-weight: 600; letter-spacing: .02em; line-height: 1.2; }
    .activity-tooltip-summary { margin-top: 10px; padding: 8px 0; border-top: 1px solid var(--line-bright); border-bottom: 1px solid var(--line-bright); color: var(--text); font: 700 14px/1.35 ui-monospace, SFMono-Regular, Menlo, monospace; }
    .activity-tooltip-note { margin-top: 8px; color: var(--warm); font-family: var(--font-display); font-size: 11px; line-height: 1.25; }
    .activity-tooltip-section { margin-top: 12px; }
    .activity-tooltip-section-title { color: var(--faint); font-family: var(--font-display); font-size: 10px; font-weight: 600; letter-spacing: .14em; line-height: 1; }
    .activity-tooltip-row { display: flex; align-items: baseline; justify-content: space-between; gap: 12px; padding: 5px 0 4px; border-bottom: 1px dotted var(--line); font: 12px/1.25 ui-monospace, SFMono-Regular, Menlo, monospace; }
    .activity-tooltip-row span:first-child { min-width: 0; overflow: hidden; color: var(--muted); text-overflow: ellipsis; white-space: nowrap; }
    .activity-tooltip-row span:last-child { flex: 0 0 auto; color: var(--accent); white-space: nowrap; }
    .content-grid { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 10px; }
    .data-panel { min-height: 0; padding: 12px 16px; }
    .section-head { display: flex; align-items: start; justify-content: space-between; gap: 20px; padding-bottom: 8px; border-bottom: 1px solid var(--line); }
    .section-title { color: var(--accent); font: 700 12px/1 var(--font-data); letter-spacing: .08em; text-transform: uppercase; }
    table { width: 100%; table-layout: fixed; border-collapse: collapse; margin-top: 2px; }
    th, td { padding: 7px 0; border-bottom: 1px solid var(--line); text-align: left; }
    th { color: var(--faint); font: 700 9px/1.2 var(--font-data); letter-spacing: .1em; text-transform: uppercase; }
    td { color: var(--muted); font: 12px/1.25 var(--font-data); }
    th:first-child, td:first-child { width: 43%; }
    th:nth-child(2), td:nth-child(2) { width: 41%; }
    th:last-child, td:last-child { width: 16%; }
    td:first-child { min-width: 0; color: var(--text); font-weight: 600; }
    .row-name { display: flex; min-width: 0; align-items: center; gap: 7px; overflow: hidden; }
    .row-name > span:last-child { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    .row-icon { display: block; flex: 0 0 17px; width: 17px; height: 17px; object-fit: contain; image-rendering: pixelated; }
    .pixel-glyph { display: grid; flex: 0 0 17px; width: 17px; height: 17px; grid-template: repeat(5, 2px) / repeat(5, 2px); place-content: center; gap: 1px; border: 1px solid var(--glyph-edge); border-radius: 3px; background: #11130f; image-rendering: pixelated; }
    .pixel-glyph i { display: block; width: 2px; height: 2px; background: transparent; }
    .pixel-glyph i.dim { background: var(--glyph-dim); }
    .pixel-glyph i.on { background: var(--glyph); }
    .pixel-glyph.palette-0 { --glyph: #a995d3; --glyph-dim: #382f49; --glyph-edge: #5f5076; }
    .pixel-glyph.palette-1 { --glyph: #7fa7c5; --glyph-dim: #293c4a; --glyph-edge: #49677b; }
    .pixel-glyph.palette-2 { --glyph: #9bbba0; --glyph-dim: #2e4232; --glyph-edge: #536f58; }
    .pixel-glyph.palette-3 { --glyph: #d2a477; --glyph-dim: #493724; --glyph-edge: #795c3c; }
    .project-glyph, .machine-glyph { border-color: transparent; border-radius: 0; background: transparent; }
    .project-glyph i.dim, .machine-glyph i.dim { background: var(--glyph-edge); }
    th:not(:first-child), td:not(:first-child) { padding-left: 6px; text-align: right; white-space: nowrap; }
    td:nth-child(2), td:last-child { overflow: hidden; text-overflow: ellipsis; }
    td:last-child { color: var(--warm); font-weight: 700; }
    .empty-row td { padding: 26px 0 8px; color: var(--faint); font-size: 13px; font-weight: 400; }
    .first-run { display: flex; align-items: center; justify-content: space-between; gap: 24px; margin-top: 16px; padding: 18px 22px; border: 1px solid rgba(155, 187, 160, .24); border-radius: 10px; background: linear-gradient(100deg, rgba(155, 187, 160, .08), rgba(155, 187, 160, .025)); }
    .first-run h2 { margin: 0 0 4px; color: var(--text); font-family: var(--font-display); font-size: 14px; font-weight: 600; letter-spacing: .01em; text-transform: none; }
    .first-run p { max-width: 560px; color: var(--muted); font-size: 13px; }
    .command { padding: 10px 12px; border: 1px solid var(--line-bright); border-radius: 7px; background: #11130f; color: var(--accent); font: 11px ui-monospace, SFMono-Regular, Menlo, monospace; white-space: nowrap; }
    footer { display: flex; justify-content: space-between; gap: 16px; padding: 14px 4px 0; color: var(--faint); font-size: 10px; }
    @media (max-width: 1050px) {
      .stat-grid { grid-template-columns: repeat(3, 1fr); }
      .content-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); }
    }
    @media (max-width: 1240px) and (min-width: 821px) {
      .power-value { column-gap: 2px; }
      .odometer-reel { flex-basis: .8em; }
      .odometer-separator { flex-basis: .18em; }
    }
    @media (max-width: 820px) {
      main { width: min(100% - 20px, 620px); margin: 10px auto; padding: 10px; }
      .topbar { display: flex; align-items: start; flex-wrap: wrap; }
      .hero, .content-grid { grid-template-columns: 1fr; }
      .power-panel, .creature-panel { min-height: 0; }
      .power-panel { min-height: 350px; }
      .stat-grid { grid-template-columns: repeat(2, 1fr); }
      .activity-panel { padding: 18px; }
    }
    @media (max-width: 500px) {
      main { width: min(100% - 12px, 420px); margin: 6px auto; padding: 6px; border-radius: 8px; }
      .nav-link { padding: 6px 7px; font-size: 10px; }
      .hero { padding-top: 14px; }
      .creature-panel { padding: 16px; }
      .power-panel, .data-panel { padding: 18px; }
      .activity-panel { padding: 16px; }
      .stat { grid-template-columns: 30px minmax(0, 1fr); gap: 8px; padding: 10px; }
      .stat-icon { width: 30px; height: 30px; }
      .power-value { column-gap: 2px; font-size: clamp(26px, 9vw, 48px); }
      .odometer-reel { flex-basis: .8em; }
      .odometer-separator { flex-basis: .18em; }
      .first-run { align-items: start; flex-direction: column; gap: 14px; }
      .command { width: 100%; overflow: auto; }
      footer { flex-direction: column; gap: 4px; }
    }
    @media (max-width: 420px) { .composition-grid { gap: 10px; } }
    @media (prefers-reduced-motion: reduce) {
      *, *::before, *::after { scroll-behavior: auto !important; transition-duration: .01ms !important; animation-duration: .01ms !important; animation-iteration-count: 1 !important; }
      .odometer-strip { transition: none; }
    }
  </style>
</head>
<body>
<main>
  <header class="topbar">
    <a class="brand" href="/" aria-label="Tokemon overview">
      <img class="brand-mark" src="/static/tokemon/token-dex.png" alt="" width="34" height="34">
      <span class="brand-name">TOKEMON</span>
    </a>
    <nav class="nav" aria-label="Primary navigation">
      <a class="nav-link active" href="/">Overview</a>
      <a class="nav-link" href="/analytics">Analytics</a>
    </nav>
    <a class="settings-link" href="/settings" aria-label="Settings" title="Settings">⚙</a>
  </header>

  <section class="hero" aria-label="Current Tokemon and lifetime usage">
    <article class="panel creature-panel" data-stage="{{.Evolution.Stage}}">
      <div class="creature-art-wrap">
        <img class="creature-art" src="{{assetPath .Evolution.Stage}}" alt="{{.Evolution.FormName}}, stage {{.Evolution.Stage}}" onerror="this.hidden=true;this.nextElementSibling.hidden=false">
        <div class="creature-fallback" hidden>STAGE {{.Evolution.Stage}}</div>
      </div>
      <div class="creature-caption">
        <div class="form-name">{{.Evolution.FormName}}</div>
        <div class="stage-chip">FORM / {{printf "%02d" .Evolution.Stage}}</div>
      </div>
    </article>

    <article class="panel power-panel">
      <div class="power-kicker">Lifetime tokens</div>
      <div class="power-value odometer" id="lifetime-counter" data-display="{{commas .LifetimeTokens}}" aria-label="{{commas .LifetimeTokens}}"><span class="odometer-static">{{commas .LifetimeTokens}}</span></div>
      <div class="power-composition" id="token-composition" aria-label="Token mix">
        <div class="composition-meter" id="live-composition-meter" aria-hidden="true">
          <span class="composition-segment uncached" id="live-uncached-segment"></span>
          <span class="composition-segment cached" id="live-cached-segment"></span>
          <span class="composition-segment output" id="live-output-segment"></span>
        </div>
        <div class="composition-primary">
          <span class="composition-item input" id="live-input" data-tooltip-kind="composition" data-tooltip="Input&#10;Usage totals are loading" aria-label="Input token details" tabindex="0"><span><i class="composition-swatch input" aria-hidden="true"></i>Input</span><strong class="mini-odometer" id="live-input-tokens" data-display="—">—</strong></span>
          <span class="composition-item output" id="live-output" data-tooltip-kind="composition" data-tooltip="Output&#10;Usage totals are loading" aria-label="Output token details" tabindex="0"><span><i class="composition-swatch output" aria-hidden="true"></i>Output</span><strong class="mini-odometer" id="live-output-tokens" data-display="—">—</strong></span>
        </div>
      </div>
      <div class="progress-block">
        <div class="progress-row"><span>Next evolution</span></div>
        <div class="progress-value"><strong id="live-remaining-copy">{{if .Evolution.TokensRemaining}}{{compactPtr .Evolution.TokensRemaining}} <small>to go</small>{{else}}Final form{{end}}</strong><strong id="live-progress-label">{{printf "%.0f" (mul .Evolution.Progress 100)}}%</strong></div>
        <div class="progress" id="live-progress" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow="{{percent .Evolution.Progress}}"><span></span></div>
      </div>
    </article>
  </section>

  <section class="stat-grid" aria-label="Usage summary">
    <article class="stat"><img class="stat-icon" src="/static/tokemon/icons/top-machine.png" alt="" width="36" height="36"><div class="stat-copy"><div class="stat-label">Top machine</div>{{if .ByMachine}}<div class="stat-value" title="{{(index .ByMachine 0).Machine}}">{{machineDisplayName $.MachineAliases (index .ByMachine 0).Machine}}</div>{{else}}<div class="stat-value muted">—</div>{{end}}</div></article>
    <article class="stat"><img class="stat-icon" src="/static/tokemon/icons/api-cost.png" alt="" width="36" height="36"><div class="stat-copy"><div class="stat-label">Est. API cost</div>{{if .EstimatedCost.PricedTokens}}<div class="stat-value">{{money .EstimatedCost.Amount}}</div>{{else}}<div class="stat-value muted">—</div>{{end}}</div></article>
    <article class="stat"><img class="stat-icon" src="/static/tokemon/icons/cache-hit.png" alt="" width="36" height="36"><div class="stat-copy"><div class="stat-label">Cache hit</div>{{if .Cache.EligibleTokens}}<div class="stat-value">{{printf "%.1f%%" (mul .Cache.HitRate 100)}}</div><small class="stat-detail">{{compact .Cache.CachedTokens}} cached · {{compact (cacheUncached .Cache)}} uncached</small>{{else}}<div class="stat-value muted">—</div>{{end}}</div></article>
    <article class="stat"><img class="stat-icon" src="/static/tokemon/icons/threads.png" alt="" width="36" height="36"><div class="stat-copy"><div class="stat-label">Threads</div><div class="stat-value">{{commas .Threads}}</div></div></article>
    <article class="stat"><img class="stat-icon" src="/static/tokemon/icons/active-days.png" alt="" width="36" height="36"><div class="stat-copy"><div class="stat-label">Active days</div><div class="stat-value">{{commas .Activity.ActiveDays}}</div></div></article>
  </section>

  <section class="panel activity-panel" aria-labelledby="activity-title">
    <div class="section-head">
      <div class="section-title" id="activity-title">Token activity</div>
      {{if .Activity.ActiveDays}}<div class="activity-legend"><span>LOW</span><span class="activity-cell level-0" aria-hidden="true"></span><span class="activity-cell level-1" aria-hidden="true"></span><span class="activity-cell level-2" aria-hidden="true"></span><span class="activity-cell level-3" aria-hidden="true"></span><span class="activity-cell level-4" aria-hidden="true"></span><span>HIGH</span></div>{{end}}
    </div>
    <div class="activity-shell" aria-label="Daily token activity for the last 53 weeks">
      <div class="activity-weekday-labels" aria-hidden="true"><span></span><span>MON</span><span></span><span>WED</span><span></span><span>FRI</span><span></span></div>
      <div class="activity-scroll">
        <div class="activity-grid">
          {{range .Activity.Weeks}}
          <div class="activity-week">
            <span class="activity-month">{{.MonthLabel}}</span>
            {{range .Days}}<span class="activity-cell level-{{.Level}}{{if .UnknownTokens}} unknown{{end}}{{if .Future}} future{{end}}" role="gridcell" {{if .Future}}aria-hidden="true"{{else}}data-tooltip="{{activityTooltip .}}" aria-label="{{activityTooltip .}}" tabindex="0"{{end}}></span>{{end}}
          </div>
          {{end}}
        </div>
      </div>
    </div>
    {{if not .Activity.ActiveDays}}
    <div class="activity-empty">No token activity in this field yet. Your Tokemon is waiting for its first training session.</div>
    {{end}}
  </section>

  <section class="content-grid" aria-label="Usage breakdowns">
    <article class="panel data-panel">
      <div class="section-head"><div class="section-title">Projects</div></div>
      <table><thead><tr><th>Project</th><th>Tokens</th><th>Share</th></tr></thead><tbody>{{range topProjects .ByProject}}<tr><td><span class="row-name">{{with projectGlyph .Project}}<span class="pixel-glyph project-glyph palette-{{.Palette}}" aria-hidden="true">{{range .Cells}}<i class="{{glyphCell .}}"></i>{{end}}</span>{{end}}<span title="{{.Project}}">{{.Project}}</span></span></td><td>{{commas .Tokens}}</td><td>{{share .Tokens $.LifetimeTokens}}</td></tr>{{else}}<tr class="empty-row"><td colspan="3">No project usage yet.</td></tr>{{end}}</tbody></table>
    </article>
    <article class="panel data-panel">
      <div class="section-head"><div class="section-title">Harnesses</div></div>
      <table><thead><tr><th>Harness</th><th>Tokens</th><th>Share</th></tr></thead><tbody>{{range topTools .ByTool}}<tr><td><span class="row-name">{{with harnessGlyph .Tool}}<span class="pixel-glyph harness-glyph preset-{{.Preset}} palette-{{.Palette}}" aria-hidden="true">{{range .Cells}}<i class="{{glyphCell .}}"></i>{{end}}</span>{{end}}<span title="{{harnessName .Tool}}">{{harnessName .Tool}}</span></span></td><td>{{commas .Tokens}}</td><td>{{share .Tokens $.LifetimeTokens}}</td></tr>{{else}}<tr class="empty-row"><td colspan="3">No harness usage yet.</td></tr>{{end}}</tbody></table>
    </article>
    <article class="panel data-panel">
      <div class="section-head"><div class="section-title">Models</div></div>
      <table><thead><tr><th>Model</th><th>Tokens</th><th>Share</th></tr></thead><tbody>{{range topModels .ByModel}}<tr><td><span class="row-name">{{with modelGlyph .Model}}<span class="pixel-glyph model-glyph palette-{{.Palette}}" aria-hidden="true">{{range .Cells}}<i class="{{glyphCell .}}"></i>{{end}}</span>{{end}}<span title="{{.Model}}">{{modelDisplayName $.ModelAliases .Model}}</span></span></td><td>{{commas .Tokens}}</td><td>{{share .Tokens $.LifetimeTokens}}</td></tr>{{else}}<tr class="empty-row"><td colspan="3">No model usage yet.</td></tr>{{end}}</tbody></table>
    </article>
    <article class="panel data-panel">
      <div class="section-head"><div class="section-title">Machines</div></div>
      <table><thead><tr><th>Machine</th><th>Tokens</th><th>Share</th></tr></thead><tbody>{{range .ByMachine}}<tr><td><span class="row-name">{{with machineGlyph .Machine}}<span class="pixel-glyph machine-glyph palette-{{.Palette}}" aria-hidden="true">{{range .Cells}}<i class="{{glyphCell .}}"></i>{{end}}</span>{{end}}<span title="{{.Machine}}">{{machineDisplayName $.MachineAliases .Machine}}</span></span></td><td>{{commas .Tokens}}</td><td>{{share .Tokens $.LifetimeTokens}}</td></tr>{{else}}<tr class="empty-row"><td colspan="3">No machines yet.</td></tr>{{end}}</tbody></table>
    </article>
  </section>

  {{if eq .LifetimeTokens 0}}
  <section class="first-run" aria-label="Getting started">
    <div><h2>The egg is waiting for its first token.</h2><p>Run the agent on a machine with Claude Code, Codex, GitHub Copilot CLI, or a configured JSONL source. Tokemon only sends usage metadata.</p></div>
    <code class="command">tokemon agent --server …</code>
  </section>
  {{end}}

  <footer><span>Local-first · no conversation content</span><span>Just for fun · not affiliated with, endorsed by, or connected to Pokémon or The Pokémon Company.</span></footer>
</main>
<script>
(() => {
  const renderOdometer = (odometer, display, animate = true) => {
    odometer.dataset.display = display;
    odometer.setAttribute('aria-label', display);
    odometer.replaceChildren();
    const fallback = document.createElement('span');
    fallback.className = 'odometer-static';
    fallback.textContent = display;
    odometer.append(fallback);
    const fit = () => {
      const width = odometer.getBoundingClientRect().width;
      const mini = odometer.classList.contains('mini-odometer');
      const spacing = mini ? Math.max(display.length - 1, 0) : 0;
      const scale = mini ? .95 : .82;
      const size = Math.max(mini ? 10 : 28, Math.min(mini ? 28 : 80, Math.max(width - spacing, 1) / Math.max(display.length * scale, 1)));
      odometer.style.fontSize = size + 'px';
    };
    fit();

    const visual = document.createDocumentFragment();
    let digitIndex = 0;
    Array.from(display).forEach((character) => {
      if (!/\d/.test(character)) {
        const separator = document.createElement('span');
        separator.className = 'odometer-separator';
        separator.textContent = character;
        visual.append(separator);
        return;
      }

      const digit = Number(character);
      const turns = 2 + (digitIndex % 3);
      const reel = document.createElement('span');
      reel.className = 'odometer-reel';
      reel.dataset.value = String(digit);
      const strip = document.createElement('span');
      strip.className = 'odometer-strip';
      strip.style.transitionDelay = (digitIndex * 55) + 'ms';
      for (let turn = 0; turn < turns; turn += 1) {
        for (let value = 0; value < 10; value += 1) {
          const item = document.createElement('span');
          item.className = 'odometer-digit';
          item.textContent = value;
          strip.append(item);
        }
      }
      const final = document.createElement('span');
      final.className = 'odometer-digit';
      final.textContent = digit;
      strip.append(final);
      reel.append(strip);
      visual.append(reel);

      if (animate) {
        requestAnimationFrame(() => {
          strip.style.transform = 'translateY(-' + (turns * 10) + 'em)';
        });
      } else {
        strip.style.transition = 'none';
        strip.style.transform = 'translateY(-' + (turns * 10) + 'em)';
      }
      digitIndex += 1;
    });
    odometer.append(visual);
    odometer.classList.add('is-ready');
    fit();
  };

  const rollOdometerReel = (reel, digit, delay) => {
    const previous = Number(reel.dataset.value || 0);
    if (previous === digit) return;
    reel.dataset.value = String(digit);
    const strip = document.createElement('span');
    strip.className = 'odometer-strip';
    strip.style.transitionDelay = delay + 'ms';
    const steps = ((digit - previous + 10) % 10) || 10;
    for (let step = 0; step <= steps; step += 1) {
      const item = document.createElement('span');
      item.className = 'odometer-digit';
      item.textContent = (previous + step) % 10;
      strip.append(item);
    }
    reel.replaceChildren(strip);
    requestAnimationFrame(() => requestAnimationFrame(() => {
      strip.style.transform = 'translateY(-' + steps + 'em)';
    }));

    let settled = false;
    const settle = () => {
      if (settled) return;
      settled = true;
      const final = document.createElement('span');
      final.className = 'odometer-digit';
      final.textContent = digit;
      reel.replaceChildren(final);
    };
    strip.addEventListener('transitionend', settle, { once: true });
    window.setTimeout(settle, 1600 + delay);
  };

  const updateOdometer = (odometer, display) => {
    const previous = odometer.dataset.display || '';
    const sameStructure = previous.length === display.length && Array.from(display).every((character, index) => {
      return /\d/.test(character) === /\d/.test(previous[index]);
    });
    if (!sameStructure) {
      renderOdometer(odometer, display, true);
      return;
    }

    odometer.dataset.display = display;
    odometer.setAttribute('aria-label', display);
    const fallback = odometer.querySelector('.odometer-static');
    if (fallback) fallback.textContent = display;
    const reels = Array.from(odometer.querySelectorAll('.odometer-reel'));
    const changed = [];
    let digitIndex = 0;
    Array.from(display).forEach((character, index) => {
      if (!/\d/.test(character)) return;
      if (character !== previous[index]) {
        changed.push({ reel: reels[digitIndex], digit: Number(character) });
      }
      digitIndex += 1;
    });
    changed.reverse().forEach((item, index) => {
      rollOdometerReel(item.reel, item.digit, index * 45);
    });
  };

    document.querySelectorAll('.odometer, .mini-odometer').forEach((odometer) => {
      renderOdometer(odometer, odometer.dataset.display || '', true);
    });
    window.addEventListener('resize', () => {
      document.querySelectorAll('.odometer, .mini-odometer').forEach((odometer) => {
        const display = odometer.dataset.display || '';
        const width = odometer.getBoundingClientRect().width;
        const mini = odometer.classList.contains('mini-odometer');
        const spacing = mini ? Math.max(display.length - 1, 0) : 0;
        const scale = mini ? .95 : .82;
        odometer.style.fontSize = Math.max(mini ? 10 : 28, Math.min(mini ? 28 : 80, Math.max(width - spacing, 1) / Math.max(display.length * scale, 1))) + 'px';
      });
  }, { passive: true });

  const number = new Intl.NumberFormat('en-US');
  const counter = document.getElementById('lifetime-counter');
  let lifetimeTokens = Number((counter.dataset.display || '0').replaceAll(',', ''));
  let currentStage = Number(document.querySelector('.creature-panel').dataset.stage);
  let polling = false;

  const updateComposition = (composition) => {
    const input = Math.max(0, Number(composition.input_tokens) || 0);
    const uncached = Math.max(0, Number(composition.uncached_input_tokens) || 0);
    const cached = Math.max(0, Number(composition.cached_input_tokens) || 0);
    const output = Math.max(0, Number(composition.output_tokens) || 0);
    const total = input + output;
    const segments = [
      ['live-uncached-segment', uncached],
      ['live-cached-segment', cached],
      ['live-output-segment', output],
    ];
    segments.forEach(([id, value]) => {
      const segment = document.getElementById(id);
      segment.hidden = value === 0;
      segment.style.width = total === 0 ? '0%' : (value / total * 100) + '%';
    });
    const setTooltip = (id, title, value, details) => {
      const item = document.getElementById(id);
      const exact = number.format(value) + ' tokens';
      item.dataset.tooltip = [title, exact, ...details].join('\n');
      const accessibleDetails = details.length ? '. ' + details.map((detail) => detail.trim().replace(' · ', ': ')).join('. ') : '';
      item.setAttribute('aria-label', title + ': ' + exact + accessibleDetails);
    };
    updateOdometer(document.getElementById('live-input-tokens'), number.format(input));
    updateOdometer(document.getElementById('live-output-tokens'), number.format(output));
    setTooltip('live-input', 'Input', input, []);
    setTooltip('live-output', 'Output', output, []);
  };

  const pollLifetime = async () => {
    if (polling || document.hidden) return;
    polling = true;
    try {
      const response = await fetch('/api/v1/evolution', { cache: 'no-store', headers: { Accept: 'application/json' } });
      if (!response.ok) throw new Error('live counter unavailable');
      const snapshot = await response.json();
      const composition = snapshot.composition || {};
      updateComposition(composition);
      if (snapshot.stage !== currentStage) {
        location.reload();
        return;
      }
      if (snapshot.lifetime_tokens === lifetimeTokens) return;
      lifetimeTokens = snapshot.lifetime_tokens;
      const display = number.format(lifetimeTokens);
      updateOdometer(counter, display);
      const remaining = document.getElementById('live-remaining-copy');
      remaining.replaceChildren();
      if (snapshot.tokens_remaining == null) {
        remaining.textContent = 'Final form';
      } else {
        remaining.append(new Intl.NumberFormat('en-US', { notation: 'compact', maximumFractionDigits: 1 }).format(snapshot.tokens_remaining) + ' ');
        const suffix = document.createElement('small');
        suffix.textContent = 'to go';
        remaining.append(suffix);
      }
      document.getElementById('live-progress-label').textContent = Math.round(snapshot.progress * 100) + '%';
      const progress = document.getElementById('live-progress');
      progress.setAttribute('aria-valuenow', String(snapshot.progress * 100));
      progress.firstElementChild.style.width = (snapshot.progress * 100) + '%';
    } catch (_) {
    } finally {
      polling = false;
    }
  };
  pollLifetime();
  window.setInterval(pollLifetime, 2000);
  document.addEventListener('visibilitychange', pollLifetime);

  const tooltip = document.createElement('div');
  tooltip.className = 'activity-tooltip';
  tooltip.setAttribute('role', 'tooltip');
  tooltip.hidden = true;
  document.body.append(tooltip);

  let activeCell = null;
  const addTooltipText = (className, text) => {
    const node = document.createElement('div');
    node.className = className;
    node.textContent = text;
    tooltip.append(node);
    return node;
  };

  const renderTooltip = (cell) => {
    tooltip.replaceChildren();
    const lines = (cell.dataset.tooltip || '').split('\n');
    addTooltipText('activity-tooltip-date', lines.shift() || '');
    addTooltipText('activity-tooltip-summary', lines.shift() || '');
    if (cell.dataset.tooltipKind === 'composition') {
      lines.forEach((line) => {
        const row = document.createElement('div');
        row.className = 'activity-tooltip-row';
        const separator = line.indexOf(' · ');
        const name = separator >= 0 ? line.slice(2, separator) : line.trim();
        const tokens = separator >= 0 ? line.slice(separator + 3) : '';
        const nameNode = document.createElement('span');
        nameNode.textContent = name;
        const tokenNode = document.createElement('span');
        tokenNode.textContent = tokens;
        row.append(nameNode, tokenNode);
        tooltip.append(row);
      });
      return;
    }
    let section = null;
    lines.forEach((line) => {
      if (line === 'Some token totals unavailable') {
        addTooltipText('activity-tooltip-note', line);
        return;
      }
      if (line === 'BY MODEL' || line === 'BY PROVIDER') {
        section = document.createElement('div');
        section.className = 'activity-tooltip-section';
        const heading = document.createElement('div');
        heading.className = 'activity-tooltip-section-title';
        heading.textContent = line;
        section.append(heading);
        tooltip.append(section);
        return;
      }
      if (!section || !line.startsWith('  ')) {
        return;
      }
      const row = document.createElement('div');
      row.className = 'activity-tooltip-row';
      const separator = line.indexOf(' · ');
      const name = separator >= 0 ? line.slice(2, separator) : line.trim();
      const tokens = separator >= 0 ? line.slice(separator + 3) : '';
      const nameNode = document.createElement('span');
      nameNode.textContent = name;
      const tokenNode = document.createElement('span');
      tokenNode.textContent = tokens;
      row.append(nameNode, tokenNode);
      section.append(row);
    });
  };

  const positionTooltip = () => {
    if (!activeCell || tooltip.hidden) {
      return;
    }
    const cell = activeCell.getBoundingClientRect();
    const margin = 12;
    const width = tooltip.offsetWidth;
    const height = tooltip.offsetHeight;
    let left = cell.left + (cell.width / 2) - (width / 2);
    let top = cell.top - height - margin;
    if (top < margin) {
      top = cell.bottom + margin;
    }
    left = Math.max(margin, Math.min(left, window.innerWidth - width - margin));
    top = Math.max(margin, Math.min(top, window.innerHeight - height - margin));
    tooltip.style.left = left + 'px';
    tooltip.style.top = top + 'px';
  };

  const showTooltip = (cell) => {
    activeCell = cell;
    renderTooltip(cell);
    tooltip.hidden = false;
    positionTooltip();
    requestAnimationFrame(positionTooltip);
  };

  const hideTooltip = () => {
    activeCell = null;
    tooltip.hidden = true;
  };

  document.querySelectorAll('.activity-cell[data-tooltip], .composition-item[data-tooltip-kind]').forEach((cell) => {
    cell.addEventListener('pointerenter', () => showTooltip(cell));
    cell.addEventListener('pointerleave', hideTooltip);
    cell.addEventListener('focus', () => showTooltip(cell));
    cell.addEventListener('blur', hideTooltip);
  });
  window.addEventListener('resize', positionTooltip, { passive: true });
  window.addEventListener('scroll', positionTooltip, { passive: true, capture: true });
})();
</script>
</body>
</html>`

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
		if day.UnknownTokens > 0 {
			tokenLabel += " known"
		}
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
		if value.UnknownTokens > 0 {
			tokenLabel += " known"
		}
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

const settingsTemplate = `{{define "settings"}}<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="theme-color" content="#10110f">
  <link rel="icon" type="image/png" href="/static/tokemon/token-dex.png">
  <title>Tokemon · Settings</title>
  <style>
    :root {
      color-scheme: dark;
      --bg: #10110f;
      --surface: #171916;
      --text: #f0ede5;
      --muted: #a2a69b;
      --faint: #6f766b;
      --line: #30352d;
      --line-bright: #485044;
      --accent: #9bbba0;
      --warm: #d2a477;
      --font-data: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace;
    }
    * { box-sizing: border-box; }
    body { margin: 0; background: var(--bg); color: var(--text); font: 14px/1.5 Inter, ui-sans-serif, system-ui, sans-serif; }
    a { color: inherit; text-decoration: none; }
    button, input { font: inherit; }
    main { width: min(900px, calc(100% - 24px)); margin: 24px auto; padding: 18px; border: 1px solid var(--line); border-radius: 12px; }
    .topbar { display: flex; align-items: center; justify-content: space-between; gap: 16px; padding-bottom: 14px; border-bottom: 1px solid var(--line); }
    .back-link { color: var(--accent); font: 12px/1 var(--font-data); letter-spacing: .08em; text-transform: uppercase; }
    h1, h2 { margin: 0; font-family: var(--font-data); }
    h1 { color: var(--accent); font-size: 16px; letter-spacing: .08em; text-transform: uppercase; }
    h2 { margin-top: 24px; padding-bottom: 8px; border-bottom: 1px solid var(--line); color: var(--accent); font-size: 12px; letter-spacing: .1em; text-transform: uppercase; }
    .intro { margin: 14px 0 0; color: var(--muted); }
    .notice { margin-top: 14px; padding: 10px 12px; border: 1px solid var(--accent); color: var(--accent); font-family: var(--font-data); font-size: 12px; }
    .alias-list { display: grid; gap: 8px; margin-top: 10px; }
    .alias-row { display: grid; grid-template-columns: minmax(0, 1fr) minmax(210px, .7fr); align-items: center; gap: 14px; padding: 10px 12px; border: 1px solid var(--line); background: var(--surface); }
    .identity { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font: 700 12px/1.25 var(--font-data); }
    .default-name { display: block; margin-top: 4px; overflow: hidden; color: var(--faint); font: 10px/1.2 var(--font-data); text-overflow: ellipsis; white-space: nowrap; }
    input { width: 100%; min-width: 0; padding: 8px 9px; border: 1px solid var(--line-bright); border-radius: 4px; background: #11130f; color: var(--text); }
    input:focus { border-color: var(--accent); outline: 2px solid rgba(155, 187, 160, .16); }
    .actions { display: flex; justify-content: flex-end; margin-top: 20px; }
    button { padding: 9px 14px; border: 1px solid var(--warm); border-radius: 4px; background: transparent; color: var(--warm); cursor: pointer; font: 700 11px/1 var(--font-data); letter-spacing: .08em; text-transform: uppercase; }
    button:hover, button:focus-visible { background: rgba(210, 164, 119, .1); outline: none; }
    .empty { margin: 12px 0 0; color: var(--faint); font-family: var(--font-data); font-size: 12px; }
    footer { margin-top: 20px; color: var(--faint); font-size: 10px; }
    @media (max-width: 620px) {
      main { margin: 8px auto; padding: 12px; }
      .alias-row { grid-template-columns: 1fr; gap: 8px; }
    }
  </style>
</head>
<body>
<main>
  <header class="topbar">
    <h1>Settings</h1>
    <a class="back-link" href="/">← Overview</a>
  </header>
  <p class="intro">Give machines and models short dashboard names. Leave an alias blank to use Tokemon’s compact default. Raw IDs remain available on hover and in the data view.</p>
  {{if .Saved}}<div class="notice" role="status">Aliases saved.</div>{{end}}
  <form action="/settings/aliases" method="post">
    <h2>Models</h2>
    {{if .Models}}
    <div class="alias-list">
      {{range $index, $row := .Models}}
      <div class="alias-row">
        <div><div class="identity" title="{{$row.Identity}}">{{$row.Identity}}</div><span class="default-name">Default: {{$row.Default}}</span></div>
        <input id="model-alias-{{$index}}" name="model_alias" value="{{$row.Alias}}" placeholder="{{$row.Default}}" maxlength="48" aria-label="Alias for {{$row.Identity}}">
        <input type="hidden" name="model_identity" value="{{$row.Identity}}">
      </div>
      {{end}}
    </div>
    {{else}}<p class="empty">No models recorded yet.</p>{{end}}
    <h2>Machines</h2>
    {{if .Machines}}
    <div class="alias-list">
      {{range $index, $row := .Machines}}
      <div class="alias-row">
        <div><div class="identity" title="{{$row.Identity}}">{{$row.Identity}}</div><span class="default-name">Default: {{$row.Default}}</span></div>
        <input id="machine-alias-{{$index}}" name="machine_alias" value="{{$row.Alias}}" placeholder="{{$row.Default}}" maxlength="48" aria-label="Alias for {{$row.Identity}}">
        <input type="hidden" name="machine_identity" value="{{$row.Identity}}">
      </div>
      {{end}}
    </div>
    {{else}}<p class="empty">No machines recorded yet.</p>{{end}}
    <div class="actions"><button type="submit">Save aliases</button></div>
  </form>
  <footer>Aliases affect dashboard presentation only; usage records and analytics identifiers remain unchanged.</footer>
</main>
</body>
</html>{{end}}`

const analyticsTemplate = `{{define "analytics"}}<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="theme-color" content="#10110f">
  <link rel="icon" type="image/png" href="/static/tokemon/token-dex.png">
  <title>Tokemon · Analytics</title>
  <style>
    @font-face {
      font-family: "Pixelify Sans";
      font-style: normal;
      font-weight: 400 700;
      font-display: swap;
      src: url("/static/tokemon/fonts/pixelify-sans-latin.woff2") format("woff2");
    }
    :root {
      color-scheme: dark;
      --bg: #10110f;
      --surface: #171916;
      --surface-raised: #1d201b;
      --text: #f0ede5;
      --muted: #a2a69b;
      --faint: #6f766b;
      --line: #30352d;
      --line-bright: #485044;
      --accent: #9bbba0;
      --accent-dim: #607864;
      --warm: #d2a477;
      --font-display: "Pixelify Sans", ui-sans-serif, system-ui, sans-serif;
      --font-data: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace;
    }
    * { box-sizing: border-box; }
    [hidden] { display: none !important; }
    body { margin: 0; background: var(--bg); color: var(--text); font: 14px/1.5 Inter, ui-sans-serif, system-ui, sans-serif; }
    a { color: inherit; text-decoration: none; }
    button, select { font: inherit; }
    main { width: min(1408px, calc(100% - 40px)); margin: 20px auto; padding: 14px; border: 1px solid var(--line); border-radius: 12px; }
    .topbar { display: grid; align-items: center; grid-template-columns: 1fr auto 1fr; gap: 24px; padding: 0 10px 14px; border-bottom: 1px solid var(--line); }
    .brand { display: flex; align-items: center; gap: 12px; }
    .brand-mark { display: block; width: 34px; height: 34px; object-fit: contain; image-rendering: pixelated; }
    .brand-name { font-family: var(--font-display); font-size: 16px; font-weight: 700; letter-spacing: .08em; }
    .nav { display: flex; align-items: center; grid-column: 2; gap: 18px; }
    .nav-link { padding: 7px 11px 6px; border-bottom: 2px solid transparent; color: var(--muted); font: 12px/1 var(--font-data); letter-spacing: .08em; text-transform: uppercase; }
    .nav-link.active { border-bottom-color: var(--accent); color: var(--accent); }
    .analytics-heading { display: flex; align-items: end; justify-content: space-between; gap: 20px; padding: 24px 10px 16px; }
    .eyebrow, .section-title, .stat-label, label, th { color: var(--accent); font: 700 10px/1 var(--font-data); letter-spacing: .1em; text-transform: uppercase; }
    h1 { margin: 7px 0 0; color: var(--text); font-family: var(--font-display); font-size: clamp(28px, 4vw, 42px); line-height: 1; }
    .heading-copy { max-width: 700px; margin: 9px 0 0; color: var(--muted); }
    .heading-actions { display: flex; align-items: center; gap: 10px; }
    .window-nav { display: inline-flex; align-items: center; padding: 3px; border: 1px solid var(--line-bright); border-radius: 5px; background: var(--surface); }
    .window-link { min-width: 42px; padding: 8px 9px; border-radius: 3px; color: var(--faint); font: 700 10px/1 var(--font-data); letter-spacing: .06em; text-align: center; }
    .window-link:hover, .window-link:focus-visible { color: var(--text); outline: 2px solid rgba(155, 187, 160, .18); outline-offset: 1px; }
    .window-link.active { background: var(--accent); color: var(--bg); }
    .action { display: inline-flex; align-items: center; justify-content: center; min-height: 38px; padding: 10px 13px; border: 1px solid var(--warm); border-radius: 5px; color: var(--warm); font: 700 11px/1 var(--font-data); letter-spacing: .07em; text-transform: uppercase; white-space: nowrap; }
    .action:hover, .action:focus-visible { background: rgba(210, 164, 119, .1); outline: 2px solid rgba(210, 164, 119, .22); outline-offset: 2px; }
    .panel { border: 1px solid var(--line-bright); border-radius: 8px; background: var(--surface); }
    .filter-panel { padding: 14px 16px; }
    .filter-form { display: grid; grid-template-columns: repeat(6, minmax(0, 1fr)); align-items: end; gap: 10px; }
    label { display: grid; gap: 7px; color: var(--faint); }
    select { width: 100%; min-width: 0; padding: 9px 10px; border: 1px solid var(--line-bright); border-radius: 4px; background: #11130f; color: var(--text); }
    select:focus-visible { border-color: var(--accent); outline: 2px solid rgba(155, 187, 160, .16); }
    .filter-actions { display: flex; align-items: end; gap: 8px; }
    .filter-actions .action { min-height: 36px; padding: 9px 11px; border-color: var(--line-bright); color: var(--muted); }
    .filter-actions .apply { border-color: var(--accent); color: var(--accent); }
    .stats { display: grid; grid-template-columns: repeat(5, minmax(0, 1fr)); gap: 10px; margin-top: 10px; }
    .stat { min-width: 0; min-height: 94px; padding: 13px 14px; border: 1px solid var(--line-bright); border-radius: 7px; background: var(--surface); animation: stat-in 300ms both; }
    .stat:nth-child(2) { animation-delay: 25ms; }
    .stat:nth-child(3) { animation-delay: 50ms; }
    .stat:nth-child(4) { animation-delay: 75ms; }
    .stat:nth-child(5) { animation-delay: 100ms; }
    .stat-value { margin-top: 9px; overflow: hidden; color: var(--text); font: 700 clamp(18px, 2.3vw, 26px)/1 var(--font-data); text-overflow: ellipsis; white-space: nowrap; }
    .stat-value.muted { color: var(--muted); font-size: 16px; }
    .stat-meta { margin-top: 7px; overflow: hidden; color: var(--faint); font: 10px/1.1 var(--font-data); text-overflow: ellipsis; white-space: nowrap; }
    .comparison { display: inline-flex; align-items: center; gap: 5px; color: var(--muted); }
    .comparison strong { color: var(--text); font-weight: 700; }
    .comparison.up strong { color: var(--accent); }
    .comparison.down strong { color: var(--warm); }
    .note { margin-top: 10px; padding: 9px 12px; border: 1px solid rgba(210, 164, 119, .45); color: var(--warm); font: 11px/1.3 var(--font-data); }
    .analytics-grid { display: grid; grid-template-columns: minmax(0, 1.2fr) minmax(360px, .8fr); align-items: start; gap: 10px; margin-top: 10px; }
    .section-head { display: flex; align-items: start; justify-content: space-between; gap: 14px; padding: 13px 16px 10px; border-bottom: 1px solid var(--line); }
    .section-meta { color: var(--faint); font: 10px/1.2 var(--font-data); text-align: right; }
    .trend-legend { display: flex; flex-wrap: wrap; gap: 10px; margin: 12px 16px 0; color: var(--muted); font: 10px/1 var(--font-data); }
    .legend-item { display: inline-flex; align-items: center; gap: 5px; }
    .legend-swatch { width: 9px; height: 9px; border-radius: 2px; background: var(--accent-dim); }
    .legend-swatch.cached { background: var(--accent); }
    .legend-swatch.output { background: var(--warm); }
    .legend-swatch.unknown { border: 1px solid var(--warm); background: transparent; }
    .trend-readout { display: grid; grid-template-columns: auto minmax(0, 1fr) auto; align-items: center; gap: 12px; min-height: 50px; margin: 12px 16px 0; padding: 9px 11px; border: 1px solid var(--line); border-radius: 5px; background: var(--surface-raised); }
    .readout-kicker { color: var(--faint); font: 700 9px/1 var(--font-data); letter-spacing: .09em; text-transform: uppercase; }
    .readout-value { overflow: hidden; color: var(--text); font: 700 15px/1.1 var(--font-data); text-overflow: ellipsis; white-space: nowrap; }
    .readout-mix { overflow: hidden; color: var(--muted); font: 10px/1.2 var(--font-data); text-align: right; text-overflow: ellipsis; white-space: nowrap; }
    .trend-visual { position: relative; border-bottom: 1px solid var(--line); }
    .trend-grid { position: absolute; z-index: 0; inset: 18px 16px 39px; pointer-events: none; }
    .trend-grid i { position: absolute; right: 0; left: 0; border-top: 1px solid rgba(72, 80, 68, .38); }
    .trend-grid i:nth-child(1) { top: 0; }
    .trend-grid i:nth-child(2) { top: 25%; }
    .trend-grid i:nth-child(3) { top: 50%; }
    .trend-grid i:nth-child(4) { top: 75%; }
    .trend-chart { position: relative; z-index: 1; display: flex; min-height: 252px; align-items: end; gap: 4px; margin-top: 8px; padding: 12px 16px 16px; overflow-x: auto; }
    .trend-column { position: relative; display: flex; min-width: 12px; flex: 1 0 12px; flex-direction: column; justify-content: end; gap: 6px; min-height: 220px; padding: 0; border: 0; background: transparent; color: inherit; cursor: crosshair; }
    .trend-column:focus-visible { border-radius: 3px; outline: 2px solid var(--accent); outline-offset: 2px; }
    .trend-column.unknown::after { position: absolute; right: 0; bottom: 19px; left: 0; height: 3px; border: 1px solid var(--warm); background: transparent; content: ""; }
    .trend-track { position: relative; display: flex; height: 190px; align-items: end; }
    .trend-bar { display: flex; width: 100%; min-height: 2px; flex-direction: column-reverse; overflow: hidden; border: 1px solid var(--line-bright); border-radius: 2px 2px 0 0; background: #20251e; transform-origin: bottom; animation: chart-rise 460ms cubic-bezier(.2, .75, .25, 1) both; animation-delay: calc(var(--index) * 6ms); }
    .trend-column.unknown .trend-bar { border-color: var(--warm); }
    .trend-part { display: block; min-height: 0; }
    .trend-part.input { background: var(--accent-dim); }
    .trend-part.cached { background: var(--accent); }
    .trend-part.output { background: var(--warm); }
    .trend-label { overflow: hidden; color: var(--faint); font: 9px/1 var(--font-data); opacity: 0; text-align: center; text-overflow: ellipsis; white-space: nowrap; }
    .period-7d .trend-label, .period-24h .trend-column:nth-child(4n + 1) .trend-label, .period-30d .trend-column:nth-child(5n + 1) .trend-label, .period-90d .trend-column:nth-child(15n + 1) .trend-label, .period-all .trend-column:nth-child(3n + 1) .trend-label { opacity: 1; }
    .empty-chart { display: grid; min-height: 220px; place-items: center; color: var(--faint); font: 12px var(--font-data); }
    .trend-panel, .breakdown-panel, .sessions-panel { min-width: 0; overflow: hidden; }
    .dimension-nav { display: flex; gap: 6px; padding: 12px 16px 0; overflow-x: auto; }
    .dimension-link { padding: 7px 9px; border: 1px solid var(--line); border-radius: 4px; color: var(--muted); font: 10px/1 var(--font-data); letter-spacing: .07em; text-transform: uppercase; white-space: nowrap; }
    .dimension-link.active { border-color: var(--accent); color: var(--accent); }
    .table-scroll { overflow-x: auto; padding: 0 16px 12px; }
    .breakdown-scroll { max-height: 350px; overflow-y: auto; scrollbar-color: var(--line-bright) transparent; }
    table { width: 100%; min-width: 430px; border-collapse: collapse; table-layout: fixed; }
    th, td { padding: 8px 0; border-bottom: 1px solid var(--line); text-align: left; }
    th { color: var(--faint); font-size: 9px; }
    .breakdown-scroll th { position: sticky; z-index: 2; top: 0; background: var(--surface); }
    td { color: var(--muted); font: 12px/1.25 var(--font-data); }
    th:first-child, td:first-child { width: 42%; }
    th:not(:first-child), td:not(:first-child) { padding-left: 9px; text-align: right; white-space: nowrap; }
    td:first-child { overflow: hidden; color: var(--text); font-weight: 600; text-overflow: ellipsis; white-space: nowrap; }
    tbody tr { transition: background-color 140ms ease; }
    tbody tr:hover { background: rgba(155, 187, 160, .035); }
    .breakdown-name { display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    .breakdown-meter { display: block; height: 2px; margin-top: 5px; overflow: hidden; background: var(--line); }
    .breakdown-meter span { display: block; width: var(--share); height: 100%; background: var(--accent-dim); animation: meter-grow 680ms cubic-bezier(.2, .75, .25, 1) both; }
    .empty-row td { padding: 30px 0 12px; color: var(--faint); text-align: left; }
    .session-project { display: block; overflow: hidden; color: var(--text); text-overflow: ellipsis; white-space: nowrap; }
    .session-machine { display: block; margin-top: 3px; overflow: hidden; color: var(--faint); font-size: 10px; font-weight: 400; text-overflow: ellipsis; white-space: nowrap; }
    .unknown-flag { display: inline-grid; width: 14px; height: 14px; place-items: center; margin-left: 4px; border: 1px solid var(--warm); border-radius: 50%; color: var(--warm); font-size: 9px; }
    footer { display: flex; justify-content: space-between; gap: 16px; padding: 14px 4px 0; color: var(--faint); font-size: 10px; }
    @keyframes chart-rise { from { opacity: .65; transform: scaleY(.12); } to { opacity: 1; transform: scaleY(1); } }
    @keyframes meter-grow { from { width: 0; } }
    @keyframes stat-in { from { opacity: .72; transform: translateY(3px); } to { opacity: 1; transform: translateY(0); } }
    @media (max-width: 1080px) {
      .filter-form { grid-template-columns: repeat(3, minmax(0, 1fr)); }
      .filter-actions { grid-column: span 3; }
      .stats { grid-template-columns: repeat(3, minmax(0, 1fr)); }
      .analytics-grid { grid-template-columns: minmax(0, 1fr); }
    }
    @media (max-width: 700px) {
      main { width: min(100% - 12px, 620px); margin: 6px auto; padding: 6px; border-radius: 8px; }
      .topbar { display: flex; align-items: start; flex-wrap: wrap; }
      .nav { order: 3; width: 100%; justify-content: center; }
      .analytics-heading { align-items: start; flex-direction: column; padding: 18px 8px 14px; }
      .heading-actions { width: 100%; align-items: stretch; flex-direction: column; }
      .window-nav { display: grid; grid-template-columns: repeat(5, 1fr); }
      .action { width: 100%; }
      .filter-panel { padding: 13px 12px; }
      .filter-form { grid-template-columns: repeat(2, minmax(0, 1fr)); }
      .filter-actions { grid-column: span 2; }
      .filter-actions .action { width: auto; }
      .stats { grid-template-columns: repeat(2, minmax(0, 1fr)); }
      .section-head { align-items: start; flex-direction: column; }
      .section-meta { text-align: left; }
      .trend-readout { grid-template-columns: 1fr; gap: 5px; }
      .readout-mix { text-align: left; white-space: normal; }
      footer { flex-direction: column; gap: 4px; }
    }
    @media (max-width: 430px) {
      .filter-form, .stats { grid-template-columns: 1fr; }
      .filter-actions { grid-column: auto; }
      .filter-actions .action { flex: 1; }
    }
    @media (prefers-reduced-motion: reduce) { *, *::before, *::after { scroll-behavior: auto !important; transition-duration: .01ms !important; animation-duration: .01ms !important; animation-delay: 0ms !important; } }
  </style>
</head>
<body>
<main>
  <header class="topbar">
    <a class="brand" href="/" aria-label="Tokemon overview">
      <img class="brand-mark" src="/static/tokemon/token-dex.png" alt="" width="34" height="34">
      <span class="brand-name">TOKEMON</span>
    </a>
    <nav class="nav" aria-label="Primary navigation">
      <a class="nav-link" href="/">Overview</a>
      <a class="nav-link active" href="/analytics" aria-current="page">Analytics</a>
    </nav>
  </header>

  <section class="analytics-heading">
    <div>
      <div class="eyebrow">Detailed usage</div>
      <h1>Analytics</h1>
      <p class="heading-copy">Explore token volume over time and see which projects, harnesses, models, and machines are carrying the load.</p>
    </div>
    <div class="heading-actions">
      <nav class="window-nav" aria-label="Time window">
        <a class="window-link{{if eq .Filter.Period "24h"}} active{{end}}" href="{{analyticsPeriodURL .Filter "24h"}}"{{if eq .Filter.Period "24h"}} aria-current="page"{{end}}>24H</a>
        <a class="window-link{{if eq .Filter.Period "7d"}} active{{end}}" href="{{analyticsPeriodURL .Filter "7d"}}"{{if eq .Filter.Period "7d"}} aria-current="page"{{end}}>7D</a>
        <a class="window-link{{if eq .Filter.Period "30d"}} active{{end}}" href="{{analyticsPeriodURL .Filter "30d"}}"{{if eq .Filter.Period "30d"}} aria-current="page"{{end}}>30D</a>
        <a class="window-link{{if eq .Filter.Period "90d"}} active{{end}}" href="{{analyticsPeriodURL .Filter "90d"}}"{{if eq .Filter.Period "90d"}} aria-current="page"{{end}}>90D</a>
        <a class="window-link{{if eq .Filter.Period "all"}} active{{end}}" href="{{analyticsPeriodURL .Filter "all"}}"{{if eq .Filter.Period "all"}} aria-current="page"{{end}}>ALL</a>
      </nav>
      <a class="action" href="{{.ExportURL}}">Export analytics JSON</a>
    </div>
  </section>

  <section class="panel filter-panel" aria-label="Analytics filters">
    <form class="filter-form" method="get" action="/analytics">
      <input type="hidden" name="period" value="{{.Filter.Period}}">
      <label>Breakdown
        <select name="dimension">
          <option value="projects"{{if eq .Filter.Dimension "projects"}} selected{{end}}>Projects</option>
          <option value="harnesses"{{if eq .Filter.Dimension "harnesses"}} selected{{end}}>Harnesses</option>
          <option value="providers"{{if eq .Filter.Dimension "providers"}} selected{{end}}>Providers</option>
          <option value="models"{{if eq .Filter.Dimension "models"}} selected{{end}}>Models</option>
          <option value="machines"{{if eq .Filter.Dimension "machines"}} selected{{end}}>Machines</option>
        </select>
      </label>
      <label>Machine
        <select name="machine">
          <option value="">All machines</option>
          {{range .Facets.Machines}}<option value="{{.}}"{{if eq $.Filter.Machine .}} selected{{end}}>{{machineDisplayName $.MachineAliases .}}</option>{{end}}
        </select>
      </label>
      <label>Provider
        <select name="provider">
          <option value="">All providers</option>
          {{range .Facets.Providers}}<option value="{{.}}"{{if eq $.Filter.Provider .}} selected{{end}}>{{.}}</option>{{end}}
        </select>
      </label>
      <label>Model
        <select name="model">
          <option value="">All models</option>
          {{range .Facets.Models}}<option value="{{.}}"{{if eq $.Filter.Model .}} selected{{end}}>{{modelDisplayName $.ModelAliases .}}</option>{{end}}
        </select>
      </label>
      <label>Harness
        <select name="tool">
          <option value="">All harnesses</option>
          {{range .Facets.Tools}}<option value="{{.}}"{{if eq $.Filter.Tool .}} selected{{end}}>{{harnessName .}}</option>{{end}}
        </select>
      </label>
      <div class="filter-actions">
        <button class="action apply" type="submit">Apply filters</button>
        <a class="action" href="/analytics">Reset</a>
      </div>
    </form>
  </section>

  <section class="stats" aria-label="Period summary">
    <article class="stat"><div class="stat-label">Period tokens</div><div class="stat-value" data-count="{{.Summary.Tokens}}">{{commas .Summary.Tokens}}</div>{{if .Comparison}}{{if .Comparison.TokenChangePercent}}<div class="stat-meta comparison {{analyticsChangeClass .Comparison.TokenChangePercent}}"><strong>{{analyticsChange .Comparison.TokenChangePercent}}</strong><span>vs previous window</span></div>{{else}}<div class="stat-meta">No earlier baseline</div>{{end}}{{else}}<div class="stat-meta">{{.StartDate}} → {{.EndDate}}</div>{{end}}</article>
    <article class="stat"><div class="stat-label">Active days</div><div class="stat-value" data-count="{{.Summary.ActiveDays}}">{{commas .Summary.ActiveDays}}</div><div class="stat-meta">{{.StartDate}} → {{.EndDate}}</div></article>
    <article class="stat"><div class="stat-label">Average thread</div><div class="stat-value" data-count="{{.AverageThread}}">{{commas .AverageThread}}</div><div class="stat-meta">Across {{commas .Summary.Threads}} thread{{if ne .Summary.Threads 1}}s{{end}}</div></article>
    <article class="stat"><div class="stat-label">Cache hit</div>{{if .Summary.Cache.EligibleTokens}}<div class="stat-value">{{printf "%.1f%%" (percent .Summary.Cache.HitRate)}}</div><div class="stat-meta">{{commas .Summary.Cache.CachedTokens}} cached tokens</div>{{else}}<div class="stat-value muted">—</div><div class="stat-meta">No eligible input tokens</div>{{end}}</article>
    <article class="stat"><div class="stat-label">Est. API cost</div>{{if .Summary.EstimatedCost.PricedTokens}}<div class="stat-value">{{money .Summary.EstimatedCost.Amount}}</div><div class="stat-meta">Priced tokens: {{commas .Summary.EstimatedCost.PricedTokens}}</div>{{else}}<div class="stat-value muted">—</div><div class="stat-meta">No priced tokens</div>{{end}}</article>
  </section>
  {{if .Summary.UnknownEvents}}<div class="note" role="status">{{commas .Summary.UnknownEvents}} event{{if ne .Summary.UnknownEvents 1}}s{{end}} in this view have unavailable token totals. They remain visible as unknown.</div>{{end}}

  <section class="analytics-grid">
    <article class="panel trend-panel" aria-labelledby="trend-title">
      <div class="section-head"><div class="section-title" id="trend-title">Token trend</div><div class="section-meta">{{if eq .Bucket "month"}}Monthly{{else if eq .Bucket "hour"}}Hourly{{else}}Daily{{end}} buckets · {{commas .Summary.Events}} events</div></div>
      <div class="trend-legend" aria-label="Token trend legend"><span class="legend-item"><i class="legend-swatch"></i>Input</span><span class="legend-item"><i class="legend-swatch cached"></i>Cached</span><span class="legend-item"><i class="legend-swatch output"></i>Output</span><span class="legend-item"><i class="legend-swatch unknown"></i>Unknown total</span></div>
      {{if .Points}}
      <div class="trend-readout" aria-live="polite">
        <span class="readout-kicker">Selected bucket</span>
        <strong class="readout-value" id="trend-readout-value">{{.PeakPoint.Label}} · {{commas .PeakPoint.Tokens}} tokens</strong>
        <span class="readout-mix" id="trend-readout-mix">Input {{commas .PeakPoint.InputTokens}} · Cached {{commas .PeakPoint.CachedTokens}} · Output {{commas .PeakPoint.OutputTokens}}</span>
      </div>
      <div class="trend-visual">
        <div class="trend-grid" aria-hidden="true"><i></i><i></i><i></i><i></i></div>
        <div class="trend-chart period-{{.Filter.Period}}" aria-label="Token volume over the selected window">
          {{range $index, $point := .Points}}
          <button class="trend-column{{if .UnknownEvents}} unknown{{end}}" type="button" style="--index: {{$index}}" data-label="{{.Label}}" data-tokens="{{.Tokens}}" data-input="{{.InputTokens}}" data-cached="{{.CachedTokens}}" data-output="{{.OutputTokens}}" aria-label="{{.Label}} · {{commas .Tokens}} known tokens{{if .UnknownEvents}} · unknown totals present{{end}}">
            <span class="trend-track"><span class="trend-bar" style="height: {{analyticsBar .Tokens $.MaxTokens}}%"><span class="trend-part input" style="height: {{analyticsPart .InputTokens .Tokens}}%"></span><span class="trend-part cached" style="height: {{analyticsPart .CachedTokens .Tokens}}%"></span><span class="trend-part output" style="height: {{analyticsPart .OutputTokens .Tokens}}%"></span></span></span>
            <span class="trend-label">{{.Label}}</span>
          </button>
          {{end}}
        </div>
      </div>
      {{else}}<div class="empty-chart">No token activity in this window.</div>{{end}}
    </article>

    <article class="panel breakdown-panel" aria-labelledby="breakdown-title">
      <div class="section-head"><div class="section-title" id="breakdown-title">{{if eq .Filter.Dimension "projects"}}Projects{{else if eq .Filter.Dimension "harnesses"}}Harnesses{{else if eq .Filter.Dimension "providers"}}Providers{{else if eq .Filter.Dimension "models"}}Models{{else}}Machines{{end}}</div><div class="section-meta">Token share · {{len .Breakdown}} rows</div></div>
      <nav class="dimension-nav" aria-label="Breakdown dimension">
        <a class="dimension-link{{if eq .Filter.Dimension "projects"}} active{{end}}" href="{{analyticsURL .Filter "projects"}}">Projects</a>
        <a class="dimension-link{{if eq .Filter.Dimension "harnesses"}} active{{end}}" href="{{analyticsURL .Filter "harnesses"}}">Harnesses</a>
        <a class="dimension-link{{if eq .Filter.Dimension "providers"}} active{{end}}" href="{{analyticsURL .Filter "providers"}}">Providers</a>
        <a class="dimension-link{{if eq .Filter.Dimension "models"}} active{{end}}" href="{{analyticsURL .Filter "models"}}">Models</a>
        <a class="dimension-link{{if eq .Filter.Dimension "machines"}} active{{end}}" href="{{analyticsURL .Filter "machines"}}">Machines</a>
      </nav>
      <div class="table-scroll breakdown-scroll">
        <table><thead><tr><th>Name</th><th>Tokens</th><th>Share</th><th>Threads</th></tr></thead><tbody>
          {{range .Breakdown}}
          <tr><td title="{{.Name}}"><span class="breakdown-name">{{if eq $.Filter.Dimension "models"}}{{modelDisplayName $.ModelAliases .Name}}{{else if eq $.Filter.Dimension "machines"}}{{machineDisplayName $.MachineAliases .Name}}{{else if eq $.Filter.Dimension "harnesses"}}{{harnessName .Name}}{{else}}{{.Name}}{{end}}</span><span class="breakdown-meter" aria-hidden="true"><span style="--share: {{mul .Share 100}}%"></span></span></td><td>{{commas .Tokens}}</td><td>{{printf "%.1f%%" (mul .Share 100)}}</td><td>{{commas .Sessions}}</td></tr>
          {{else}}<tr class="empty-row"><td colspan="4">No usage matches these filters.</td></tr>{{end}}
        </tbody></table>
      </div>
    </article>
  </section>

  <section class="panel sessions-panel" aria-labelledby="sessions-title">
    <div class="section-head"><div class="section-title" id="sessions-title">Recent sessions</div><div class="section-meta">Metadata only · latest 12</div></div>
    <div class="table-scroll">
      <table><thead><tr><th>When</th><th>Project / machine</th><th>Model</th><th>Harness</th><th>Tokens</th></tr></thead><tbody>
        {{range .Sessions}}
        <tr><td>{{analyticsTime .Timestamp}}</td><td><span class="session-project">{{if .Project}}{{.Project}}{{else}}Unknown project{{end}}</span><span class="session-machine">{{machineDisplayName $.MachineAliases .Machine}}</span></td><td title="{{.Model}}"><span class="session-project">{{modelDisplayName $.ModelAliases .Model}}</span><span class="session-machine">{{.Provider}}</span></td><td>{{harnessName .Tool}}</td><td>{{commas .Tokens}}{{if .UnknownEvents}}<span class="unknown-flag" title="Some token totals unavailable">?</span>{{end}}</td></tr>
        {{else}}<tr class="empty-row"><td colspan="5">No sessions match these filters.</td></tr>{{end}}
      </tbody></table>
    </div>
  </section>

  <footer><span>Local-first · metadata only</span><span>Export contains analytics metadata, not prompts, responses, source code, or repository contents.</span></footer>
</main>
<script>
  (() => {
    const number = new Intl.NumberFormat('en-US');
    const reducedMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    if (!reducedMotion) {
      document.querySelectorAll('[data-count]').forEach((node) => {
        const target = Number(node.dataset.count);
        if (!Number.isFinite(target) || target <= 0) return;
        const start = target * .92;
        const started = performance.now();
        const tick = (now) => {
          const progress = Math.min(1, (now - started) / 520);
          const eased = 1 - Math.pow(1 - progress, 3);
          node.textContent = number.format(Math.round(start + (target - start) * eased));
          if (progress < 1) requestAnimationFrame(tick);
        };
        requestAnimationFrame(tick);
      });
    }

    const value = document.getElementById('trend-readout-value');
    const mix = document.getElementById('trend-readout-mix');
    const showPoint = (column) => {
      if (!value || !mix) return;
      value.textContent = column.dataset.label + ' · ' + number.format(Number(column.dataset.tokens)) + ' tokens';
      mix.textContent = 'Input ' + number.format(Number(column.dataset.input)) + ' · Cached ' + number.format(Number(column.dataset.cached)) + ' · Output ' + number.format(Number(column.dataset.output));
    };
    document.querySelectorAll('.trend-column').forEach((column) => {
      column.addEventListener('mouseenter', () => showPoint(column));
      column.addEventListener('focus', () => showPoint(column));
      column.addEventListener('click', () => showPoint(column));
    });
  })();
</script>
</body>
</html>{{end}}`
