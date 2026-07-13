package api

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

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

const dashboardUsageRowsLimit = 5

func topProjects(values []database.ProjectTotal) []database.ProjectTotal {
	return limitDashboardRows(values)
}

func topModels(values []database.ModelTotal) []database.ModelTotal {
	return limitDashboardRows(values)
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
		"compactPtr":      compactPtr,
		"share": func(part, total int64) string {
			if total == 0 {
				return "0.0%"
			}
			return fmt.Sprintf("%.1f%%", float64(part)*100/float64(total))
		},
		"topProjects": topProjects,
		"topModels":   topModels,
		"assetPath":   func(stage int) string { return "/static/tokemon/stage-" + twoDigits(stage) + ".png" },
	}).Parse(dashboardTemplate)
	if err != nil {
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
	mux.HandleFunc("POST /api/v1/events/batch", s.ingest)
	mux.HandleFunc("GET /api/v1/evolution", s.evolution)
	mux.HandleFunc("GET /api/v1/analytics/overview", s.overview)
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
	if err := s.template.Execute(w, result); err != nil {
		return
	}
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
    main { width: min(1320px, calc(100% - 40px)); margin: 20px auto; padding: 14px; border: 1px solid var(--line); border-radius: 12px; }
    .topbar { display: grid; align-items: center; grid-template-columns: 1fr auto 1fr; gap: 24px; padding: 0 10px 14px; border-bottom: 1px solid var(--line); }
    .brand { display: flex; align-items: center; gap: 12px; }
    .brand-mark { display: block; width: 34px; height: 34px; object-fit: contain; image-rendering: pixelated; filter: drop-shadow(0 4px 6px rgba(0, 0, 0, .22)); }
    .brand-name { font-family: var(--font-display); font-size: 16px; font-weight: 700; letter-spacing: .08em; }
    .nav { display: flex; align-items: center; grid-column: 2; gap: 18px; }
    .nav-link { padding: 7px 11px 6px; border-bottom: 2px solid transparent; color: var(--muted); font: 12px/1 var(--font-data); letter-spacing: .08em; text-transform: uppercase; }
    .nav-link.active { border-bottom-color: var(--accent); color: var(--accent); }
    .hero { display: grid; grid-template-columns: minmax(280px, .82fr) minmax(0, 1.45fr); gap: 10px; padding: 10px 0; }
    .hero > .panel, .content-grid > .panel { min-width: 0; }
    .panel { border: 1px solid var(--line-bright); border-radius: 8px; background: var(--surface); box-shadow: inset 0 0 0 1px rgba(255, 255, 255, .012); }
    .creature-panel { position: relative; min-height: 398px; overflow: hidden; padding: 16px; background: radial-gradient(circle at 50% 40%, rgba(155, 187, 160, .11), transparent 48%), var(--surface); }
    .creature-art-wrap { display: grid; min-height: 285px; place-items: center; padding: 2px 20px 0; }
    .creature-art { width: min(100%, 290px); max-height: 278px; object-fit: contain; image-rendering: pixelated; filter: drop-shadow(0 20px 24px rgba(0, 0, 0, .22)); transition: transform .35s ease, filter .35s ease; }
    .creature-art:hover { transform: translateY(-5px) scale(1.02); filter: drop-shadow(0 25px 30px rgba(0, 0, 0, .32)); }
    .creature-fallback { display: grid; width: 220px; height: 220px; place-items: center; border: 1px dashed var(--line-bright); border-radius: 50%; color: var(--accent); font: 700 30px ui-monospace, SFMono-Regular, Menlo, monospace; }
    .creature-fallback[hidden] { display: none; }
    .creature-caption { display: flex; align-items: center; flex-direction: column; gap: 6px; padding-top: 3px; text-align: center; }
    .form-name { margin-top: 2px; color: var(--text); font-family: var(--font-display); font-size: clamp(24px, 3vw, 34px); font-weight: 700; letter-spacing: .02em; }
    .stage-chip { padding: 5px 10px; border: 1px solid var(--warm); border-radius: 5px; color: var(--warm); font: 600 10px/1 var(--font-data); letter-spacing: .08em; white-space: nowrap; }
    .power-panel { display: flex; min-height: 398px; flex-direction: column; padding: 16px 18px; }
    .power-kicker { color: var(--accent); font: 700 12px/1 var(--font-data); letter-spacing: .08em; text-transform: uppercase; }
    .power-value { display: flex; align-items: center; width: 100%; min-width: 0; height: 1.14em; margin: 9px 0 0; overflow: hidden; column-gap: 4px; color: var(--text); font: 700 clamp(30px, 6vw, 80px)/1 var(--font-data); font-variant-numeric: tabular-nums; letter-spacing: 0; white-space: nowrap; }
    .odometer-static { white-space: nowrap; }
    .odometer-reel { position: relative; display: block; flex: 0 0 .82em; height: 1em; overflow: hidden; border: 1px solid var(--line-bright); border-radius: 4px; background: linear-gradient(180deg, #292c26 0 49%, #1d201b 50% 100%); box-shadow: inset 0 1px rgba(255,255,255,.035), inset 0 -8px 18px rgba(0,0,0,.16); line-height: 1; }
    .odometer-reel::after { position: absolute; z-index: 2; top: 50%; right: 0; left: 0; border-top: 1px solid rgba(8, 9, 8, .65); border-bottom: 1px solid rgba(255, 255, 255, .025); content: ""; pointer-events: none; }
    .odometer-separator { display: block; flex: 0 0 .22em; color: var(--muted); line-height: 1; text-align: center; }
    .odometer-strip { display: flex; flex-direction: column; transform: translateY(0); transition: transform 1.35s cubic-bezier(.2, .75, .2, 1); will-change: transform; }
    .odometer-digit { display: grid; flex: 0 0 1em; height: 1em; place-items: center; color: var(--text); line-height: 1; text-align: center; text-shadow: 0 2px 0 rgba(0,0,0,.32); }
    .odometer.is-ready .odometer-static { display: none; }
    .power-composition { margin-top: 18px; }
    .composition-meter { display: flex; height: 26px; overflow: hidden; gap: 3px; border: 0; border-radius: 4px; background: transparent; }
    .composition-segment { display: block; min-width: 0; transition: width .45s ease; }
    .composition-segment.uncached { background: var(--accent-dim); }
    .composition-segment.cached { background: var(--accent); }
    .composition-segment.output { background: var(--warm); }
    .composition-segment.unclassified { background: repeating-linear-gradient(135deg, var(--warm) 0 3px, rgba(210, 164, 119, .28) 3px 6px); }
    .composition-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(110px, 1fr)); gap: 14px; margin-top: 8px; }
    .composition-item { display: flex; min-width: 0; align-items: center; justify-content: center; gap: 8px; color: var(--muted); font: 700 10px/1.25 var(--font-data); letter-spacing: .06em; text-transform: uppercase; }
    .composition-item strong { color: var(--text); font-size: 13px; font-weight: 700; letter-spacing: 0; }
    .composition-item.unclassified { color: var(--warm); }
    .composition-swatch { display: none; }
    .composition-swatch.output { background: var(--warm); }
    .composition-swatch.uncached { background: var(--accent-dim); }
    .composition-swatch.cached { background: var(--accent); }
    .composition-swatch.unclassified { border: 1px dashed var(--warm); background: transparent; }
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
    .activity-panel { margin-bottom: 10px; padding: 12px 16px; border-radius: 7px; }
    .activity-shell { display: flex; gap: 8px; margin-top: 7px; min-width: 0; }
    .activity-weekday-labels { display: grid; flex: 0 0 26px; grid-template-rows: 12px repeat(7, 8px); gap: 2px; color: var(--faint); font: 8px/8px var(--font-data); text-align: right; }
    .activity-weekday-labels span { height: 8px; }
    .activity-scroll { min-width: 0; flex: 1; overflow-x: auto; padding: 0 3px 7px 0; scrollbar-color: var(--line-bright) transparent; }
    .activity-grid { display: grid; width: 100%; min-width: 670px; grid-template-columns: repeat(53, minmax(9px, 1fr)); gap: 2px; }
    .activity-week { display: grid; min-width: 0; grid-template-rows: 12px repeat(7, 8px); gap: 2px; }
    .activity-month { overflow: visible; color: var(--faint); font: 8px/12px var(--font-data); white-space: nowrap; }
    .activity-cell { display: block; width: 100%; height: 8px; border: 1px solid #293127; border-radius: 0; background: #1a1f19; image-rendering: pixelated; }
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
    .content-grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 10px; }
    .data-panel { min-height: 0; padding: 12px 16px; }
    .section-head { display: flex; align-items: start; justify-content: space-between; gap: 20px; padding-bottom: 8px; border-bottom: 1px solid var(--line); }
    .section-title { color: var(--accent); font: 700 12px/1 var(--font-data); letter-spacing: .08em; text-transform: uppercase; }
    table { width: 100%; border-collapse: collapse; margin-top: 2px; }
    th, td { padding: 7px 0; border-bottom: 1px solid var(--line); text-align: left; }
    th { color: var(--faint); font: 700 9px/1.2 var(--font-data); letter-spacing: .1em; text-transform: uppercase; }
    td { color: var(--muted); font: 12px/1.25 var(--font-data); }
    td:first-child { color: var(--text); font-weight: 600; }
    .row-name { display: flex; min-width: 0; align-items: center; gap: 7px; }
    .row-icon { display: block; flex: 0 0 17px; width: 17px; height: 17px; object-fit: contain; image-rendering: pixelated; }
    th:not(:first-child), td:not(:first-child) { text-align: right; }
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
      <a class="nav-link" href="/api/v1/analytics/overview">Data</a>
    </nav>
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
          <span class="composition-segment unclassified" id="live-unclassified-segment" hidden></span>
        </div>
        <div class="composition-grid">
          <span class="composition-item"><span><i class="composition-swatch uncached" aria-hidden="true"></i>Input</span><strong id="live-uncached-tokens">—</strong></span>
          <span class="composition-item"><span><i class="composition-swatch cached" aria-hidden="true"></i>Cached</span><strong id="live-cached-tokens">—</strong></span>
          <span class="composition-item"><span><i class="composition-swatch output" aria-hidden="true"></i>Output</span><strong id="live-output-tokens">—</strong></span>
          <span class="composition-item unclassified" id="live-unclassified" hidden><span><i class="composition-swatch unclassified" aria-hidden="true"></i>Unknown</span><strong id="live-unclassified-tokens">—</strong></span>
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
    <article class="stat"><img class="stat-icon" src="/static/tokemon/icons/top-machine.png" alt="" width="36" height="36"><div class="stat-copy"><div class="stat-label">Top machine</div>{{if .ByMachine}}<div class="stat-value" title="{{(index .ByMachine 0).Machine}}">{{(index .ByMachine 0).Machine}}</div>{{else}}<div class="stat-value muted">—</div>{{end}}</div></article>
    <article class="stat"><img class="stat-icon" src="/static/tokemon/icons/api-cost.png" alt="" width="36" height="36"><div class="stat-copy"><div class="stat-label">Est. API cost</div>{{if .EstimatedCost.PricedTokens}}<div class="stat-value">{{money .EstimatedCost.Amount}}</div>{{else}}<div class="stat-value muted">—</div>{{end}}</div></article>
    <article class="stat"><img class="stat-icon" src="/static/tokemon/icons/cache-hit.png" alt="" width="36" height="36"><div class="stat-copy"><div class="stat-label">Cache hit</div>{{if .Cache.EligibleTokens}}<div class="stat-value">{{printf "%.1f%%" (mul .Cache.HitRate 100)}}</div>{{else}}<div class="stat-value muted">—</div>{{end}}</div></article>
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
      <table><thead><tr><th>Project</th><th>Tokens</th><th>Share</th></tr></thead><tbody>{{range topProjects .ByProject}}<tr><td><span class="row-name"><img class="row-icon" src="/static/tokemon/icons/project.png" alt="" width="17" height="17"><span>{{.Project}}</span></span></td><td>{{commas .Tokens}}</td><td>{{share .Tokens $.LifetimeTokens}}</td></tr>{{else}}<tr class="empty-row"><td colspan="3">No project usage yet.</td></tr>{{end}}</tbody></table>
    </article>
    <article class="panel data-panel">
      <div class="section-head"><div class="section-title">Models</div></div>
      <table><thead><tr><th>Model</th><th>Tokens</th><th>Share</th></tr></thead><tbody>{{range topModels .ByModel}}<tr><td><span class="row-name"><img class="row-icon" src="/static/tokemon/icons/model.png" alt="" width="17" height="17"><span>{{.Model}}</span></span></td><td>{{commas .Tokens}}</td><td>{{share .Tokens $.LifetimeTokens}}</td></tr>{{else}}<tr class="empty-row"><td colspan="3">No model usage yet.</td></tr>{{end}}</tbody></table>
    </article>
    <article class="panel data-panel">
      <div class="section-head"><div class="section-title">Machines</div></div>
      <table><thead><tr><th>Machine</th><th>Tokens</th><th>Share</th></tr></thead><tbody>{{range .ByMachine}}<tr><td><span class="row-name"><img class="row-icon" src="/static/tokemon/icons/machine.png" alt="" width="17" height="17"><span>{{.Machine}}</span></span></td><td>{{commas .Tokens}}</td><td>{{share .Tokens $.LifetimeTokens}}</td></tr>{{else}}<tr class="empty-row"><td colspan="3">No machines yet.</td></tr>{{end}}</tbody></table>
    </article>
  </section>

  {{if eq .LifetimeTokens 0}}
  <section class="first-run" aria-label="Getting started">
    <div><h2>The egg is waiting for its first token.</h2><p>Run the agent on a machine with Claude Code, Codex, or a configured JSONL source. Tokemon only sends usage metadata.</p></div>
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
      const size = Math.max(28, Math.min(80, width / Math.max(display.length * .82, 1)));
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

  document.querySelectorAll('.odometer').forEach((odometer) => {
    renderOdometer(odometer, odometer.dataset.display || '', true);
  });
  window.addEventListener('resize', () => {
    document.querySelectorAll('.odometer').forEach((odometer) => {
      const display = odometer.dataset.display || '';
      const width = odometer.getBoundingClientRect().width;
      odometer.style.fontSize = Math.max(28, Math.min(80, width / Math.max(display.length * .82, 1))) + 'px';
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
    const unclassified = Math.max(0, Number(composition.unclassified_tokens) || 0);
    const total = input + output + unclassified;
    const segments = [
      ['live-uncached-segment', uncached],
      ['live-cached-segment', cached],
      ['live-output-segment', output],
      ['live-unclassified-segment', unclassified],
    ];
    segments.forEach(([id, value]) => {
      const segment = document.getElementById(id);
      segment.hidden = value === 0;
      segment.style.width = total === 0 ? '0%' : (value / total * 100) + '%';
    });
    const percent = (value) => {
      if (total === 0 || value === 0) return '0%';
      const share = value / total * 100;
      return share < 1 ? '<1%' : Math.round(share) + '%';
    };
    document.getElementById('live-uncached-tokens').textContent = percent(uncached);
    document.getElementById('live-cached-tokens').textContent = percent(cached);
    document.getElementById('live-output-tokens').textContent = percent(output);
    document.getElementById('live-unclassified-tokens').textContent = percent(unclassified);
    document.getElementById('live-unclassified').hidden = unclassified === 0;
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

  document.querySelectorAll('.activity-cell[data-tooltip]').forEach((cell) => {
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

func twoDigits(value int) string {
	if value < 0 {
		value = 0
	}
	if value > 99 {
		value = 99
	}
	return strconv.FormatInt(int64(value/10), 10) + strconv.FormatInt(int64(value%10), 10)
}
