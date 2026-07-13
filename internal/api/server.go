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

func New(store *database.Store, ingestToken string) (*Server, error) {
	page, err := template.New("dashboard").Funcs(template.FuncMap{
		"commas":          commas,
		"commasPtr":       commasPtr,
		"activityTooltip": activityTooltip,
		"mul":             func(left, right float64) float64 { return left * right },
		"percent":         func(value float64) float64 { return value * 100 },
		"money":           func(value float64) string { return fmt.Sprintf("$%.2f", value) },
		"coverage": func(priced, unpriced int64) int {
			total := priced + unpriced
			if total == 0 {
				return 0
			}
			return int(float64(priced)*100/float64(total) + 0.5)
		},
		"assetPath": func(stage int) string { return "/static/tokemon/stage-" + twoDigits(stage) + ".png" },
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
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      background: var(--bg);
      color: var(--text);
      font: 14px/1.5 Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    }
    a { color: inherit; text-decoration: none; }
    button { font: inherit; }
    main { width: min(1180px, calc(100% - 40px)); margin: 0 auto; padding: 24px 0 48px; }
    .topbar { display: flex; align-items: center; justify-content: space-between; gap: 24px; padding: 0 0 20px; border-bottom: 1px solid var(--line); }
    .brand { display: flex; align-items: center; gap: 12px; }
    .brand-mark { display: block; width: 34px; height: 34px; object-fit: contain; image-rendering: pixelated; filter: drop-shadow(0 4px 6px rgba(0, 0, 0, .22)); }
    .brand-name { font-family: var(--font-display); font-size: 14px; font-weight: 650; letter-spacing: .1em; }
    .brand-subtitle { margin-left: 8px; color: var(--faint); font-size: 12px; }
    .nav { display: flex; align-items: center; gap: 6px; }
    .nav-link { padding: 7px 11px; border: 1px solid transparent; border-radius: 7px; color: var(--muted); font-size: 12px; letter-spacing: .08em; text-transform: uppercase; }
    .nav-link.active { border-color: var(--line); background: var(--surface); color: var(--text); }
    .status { display: flex; align-items: center; gap: 7px; color: var(--faint); font-size: 11px; letter-spacing: .1em; text-transform: uppercase; }
    .status-dot { width: 6px; height: 6px; border-radius: 50%; background: var(--accent); box-shadow: 0 0 0 4px rgba(155, 187, 160, .08); }
    .status.is-live .status-dot { animation: live-pulse 2s ease-out infinite; }
    .status.is-stale .status-dot { background: var(--danger); animation: none; }
    @keyframes live-pulse { 0% { box-shadow: 0 0 0 0 rgba(155, 187, 160, .32); } 70%, 100% { box-shadow: 0 0 0 7px rgba(155, 187, 160, 0); } }
    .hero { display: grid; grid-template-columns: minmax(280px, .82fr) minmax(0, 1.45fr); gap: 16px; padding: 24px 0 16px; }
    .panel { border: 1px solid var(--line); border-radius: 12px; background: var(--surface); }
    .creature-panel { position: relative; min-height: 430px; overflow: hidden; padding: 22px; background: radial-gradient(circle at 50% 46%, rgba(155, 187, 160, .09), transparent 48%), var(--surface); }
    .creature-panel::before, .creature-panel::after { position: absolute; color: var(--line-bright); font: 11px ui-monospace, SFMono-Regular, Menlo, monospace; letter-spacing: .1em; }
    .creature-panel::before { top: 74px; left: 22px; content: "///"; }
    .creature-panel::after { right: 22px; bottom: 24px; content: "STAGE " attr(data-stage); font-family: var(--font-display); font-weight: 600; letter-spacing: .08em; }
    .eyebrow { color: var(--muted); font-size: 11px; font-weight: 650; letter-spacing: .14em; text-transform: uppercase; }
    .eyebrow span { color: var(--accent); }
    .creature-art-wrap { display: grid; min-height: 310px; place-items: center; padding: 18px 20px 4px; }
    .creature-art { width: min(100%, 300px); max-height: 300px; object-fit: contain; image-rendering: pixelated; filter: drop-shadow(0 20px 24px rgba(0, 0, 0, .22)); transition: transform .35s ease, filter .35s ease; }
    .creature-art:hover { transform: translateY(-5px) scale(1.02); filter: drop-shadow(0 25px 30px rgba(0, 0, 0, .32)); }
    .creature-fallback { display: grid; width: 220px; height: 220px; place-items: center; border: 1px dashed var(--line-bright); border-radius: 50%; color: var(--accent); font: 700 30px ui-monospace, SFMono-Regular, Menlo, monospace; }
    .creature-fallback[hidden] { display: none; }
    .creature-caption { display: flex; align-items: end; justify-content: space-between; gap: 16px; border-top: 1px solid var(--line); padding-top: 16px; }
    .form-name { margin-top: 2px; color: var(--accent); font-family: var(--font-display); font-size: clamp(24px, 3vw, 36px); font-weight: 600; letter-spacing: -.02em; }
    .stage-chip { padding: 6px 9px; border: 1px solid rgba(155, 187, 160, .28); border-radius: 999px; color: var(--accent); font-family: var(--font-display); font-size: 10px; font-weight: 600; letter-spacing: .08em; white-space: nowrap; }
    .power-panel { display: flex; min-height: 430px; flex-direction: column; padding: 26px 28px; }
    .power-header { display: flex; align-items: start; justify-content: space-between; gap: 20px; }
    .power-header > div:first-child { min-width: 0; flex: 1 1 auto; }
    .power-kicker { color: var(--faint); font-size: 11px; font-weight: 650; letter-spacing: .14em; text-transform: uppercase; }
    .power-value { display: flex; align-items: center; width: 100%; min-width: 0; height: 1em; margin: 14px 0 4px; overflow: hidden; color: var(--text); font: 700 clamp(30px, 7vw, 86px)/1 ui-monospace, SFMono-Regular, Menlo, monospace; font-variant-numeric: tabular-nums; letter-spacing: -.08em; white-space: nowrap; }
    .odometer-static { white-space: nowrap; }
    .odometer-reel { display: block; flex: 0 0 .64em; height: 1em; overflow: hidden; line-height: 1; }
    .odometer-separator { display: block; flex: 0 0 .34em; line-height: 1; text-align: center; }
    .odometer-strip { display: flex; flex-direction: column; transform: translateY(0); transition: transform 1.35s cubic-bezier(.2, .75, .2, 1); will-change: transform; }
    .odometer-digit { display: block; flex: 0 0 1em; height: 1em; line-height: 1; text-align: center; }
    .odometer.is-ready .odometer-static { display: none; }
    .power-unit { color: var(--muted); font-size: 13px; }
    .power-composition { display: flex; flex-wrap: wrap; gap: 5px 14px; margin-top: 9px; color: var(--faint); font: 11px/1.4 ui-monospace, SFMono-Regular, Menlo, monospace; font-variant-numeric: tabular-nums; }
    .power-composition strong { color: var(--muted); font-weight: 600; }
    .power-composition .unclassified { color: var(--warm); }
    .power-note { flex: 0 1 230px; max-width: 230px; color: var(--faint); font-size: 12px; line-height: 1.45; text-align: right; }
    .power-note strong { display: block; margin-bottom: 3px; color: var(--warm); font-size: 13px; }
    .progress-block { margin-top: auto; padding-top: 32px; }
    .progress-row { display: flex; align-items: center; justify-content: space-between; gap: 16px; color: var(--muted); font-size: 12px; }
    .progress-row strong { color: var(--text); font-weight: 600; }
    .progress { height: 10px; margin: 11px 0 12px; overflow: hidden; border: 1px solid var(--line); border-radius: 999px; background: #11130f; }
    .progress span { display: block; width: {{percent .Evolution.Progress}}%; height: 100%; border-radius: inherit; background: linear-gradient(90deg, var(--accent-dim), var(--accent)); box-shadow: 0 0 18px rgba(155, 187, 160, .2); }
    .thresholds { display: flex; justify-content: space-between; color: var(--faint); font: 11px ui-monospace, SFMono-Regular, Menlo, monospace; }
    .stat-grid { display: grid; grid-template-columns: repeat(6, minmax(0, 1fr)); gap: 10px; padding: 0 0 16px; }
    .stat { min-height: 92px; padding: 15px 16px; border: 1px solid var(--line); border-radius: 10px; background: var(--surface); }
    .stat-label { color: var(--faint); font-size: 10px; font-weight: 650; letter-spacing: .13em; text-transform: uppercase; }
    .stat-value { margin-top: 11px; color: var(--text); font-size: 16px; font-weight: 650; overflow-wrap: anywhere; }
    .stat-value.muted { color: var(--muted); font-size: 13px; font-weight: 500; }
    .stat-detail { margin-top: 2px; color: var(--faint); font-size: 11px; }
    .activity-panel { margin-bottom: 16px; padding: 22px; border-radius: 8px; }
    .activity-summary { display: flex; align-items: end; gap: 24px; padding: 16px 0 2px; }
    .activity-summary strong { display: block; color: var(--accent); font: 700 18px/1 ui-monospace, SFMono-Regular, Menlo, monospace; }
    .activity-summary span { display: block; margin-top: 4px; color: var(--faint); font-size: 10px; letter-spacing: .1em; text-transform: uppercase; }
    .activity-window { margin-left: auto; color: var(--faint); font: 10px ui-monospace, SFMono-Regular, Menlo, monospace; white-space: nowrap; }
    .activity-shell { display: flex; gap: 10px; margin-top: 12px; min-width: 0; }
    .activity-weekday-labels { display: grid; flex: 0 0 28px; grid-template-rows: 18px repeat(7, 13px); gap: 4px; color: var(--faint); font: 9px/13px ui-monospace, SFMono-Regular, Menlo, monospace; text-align: right; }
    .activity-weekday-labels span { height: 13px; }
    .activity-scroll { min-width: 0; overflow-x: auto; padding: 0 3px 7px 0; scrollbar-color: var(--line-bright) transparent; }
    .activity-grid { display: flex; width: max-content; gap: 4px; }
    .activity-week { display: grid; grid-template-rows: 18px repeat(7, 13px); gap: 4px; min-width: 11px; }
    .activity-month { overflow: visible; color: var(--faint); font: 9px/18px ui-monospace, SFMono-Regular, Menlo, monospace; white-space: nowrap; }
    .activity-cell { display: block; width: 11px; height: 13px; border: 1px solid #293127; border-radius: 0; background: #1a1f19; box-shadow: 1px 1px 0 #0d100d; image-rendering: pixelated; }
    .activity-cell.level-1 { border-color: #36533b; background: #2a4430; }
    .activity-cell.level-2 { border-color: #4f724c; background: #416b46; }
    .activity-cell.level-3 { border-color: #779b5f; background: #6b8d56; }
    .activity-cell.level-4 { border-color: #aecb79; background: #9bb76d; }
    .activity-cell.unknown { border-style: dashed; }
    .activity-cell.future { border-color: transparent; background: transparent; box-shadow: none; }
    .activity-cell:not(.future) { cursor: pointer; }
    .activity-cell:not(.future):hover, .activity-cell:not(.future):focus-visible { position: relative; z-index: 1; outline: 2px solid var(--warm); outline-offset: 2px; }
    .activity-legend { display: flex; align-items: center; gap: 5px; margin-top: 5px; color: var(--faint); font: 10px ui-monospace, SFMono-Regular, Menlo, monospace; }
    .activity-legend .activity-cell { width: 11px; height: 11px; }
    .activity-note { margin-left: auto; font-family: inherit; text-align: right; }
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
    .content-grid { display: grid; grid-template-columns: 1.15fr .85fr; gap: 16px; }
    .data-panel { min-height: 250px; padding: 22px; }
    .section-head { display: flex; align-items: start; justify-content: space-between; gap: 20px; padding-bottom: 14px; border-bottom: 1px solid var(--line); }
    .section-title { font-family: var(--font-display); font-size: 12px; font-weight: 600; letter-spacing: .08em; text-transform: uppercase; }
    .section-subtitle { margin-top: 3px; color: var(--faint); font-size: 12px; }
    .section-mark { color: var(--accent-dim); font: 12px ui-monospace, SFMono-Regular, Menlo, monospace; }
    table { width: 100%; border-collapse: collapse; margin-top: 2px; }
    th, td { padding: 13px 0; border-bottom: 1px solid var(--line); text-align: left; }
    th { color: var(--faint); font-size: 10px; font-weight: 650; letter-spacing: .12em; text-transform: uppercase; }
    td { color: var(--muted); font-size: 13px; }
    td:first-child { color: var(--text); font-weight: 600; }
    td:last-child, th:last-child { text-align: right; }
    .empty-row td { padding: 26px 0 8px; color: var(--faint); font-size: 13px; font-weight: 400; }
    .first-run { display: flex; align-items: center; justify-content: space-between; gap: 24px; margin-top: 16px; padding: 18px 22px; border: 1px solid rgba(155, 187, 160, .24); border-radius: 10px; background: linear-gradient(100deg, rgba(155, 187, 160, .08), rgba(155, 187, 160, .025)); }
    .first-run h2 { margin: 0 0 4px; color: var(--text); font-family: var(--font-display); font-size: 14px; font-weight: 600; letter-spacing: .01em; text-transform: none; }
    .first-run p { max-width: 560px; color: var(--muted); font-size: 13px; }
    .command { padding: 10px 12px; border: 1px solid var(--line-bright); border-radius: 7px; background: #11130f; color: var(--accent); font: 11px ui-monospace, SFMono-Regular, Menlo, monospace; white-space: nowrap; }
    footer { display: flex; justify-content: space-between; gap: 16px; padding-top: 24px; color: var(--faint); font-size: 11px; }
    @media (max-width: 1050px) {
      .stat-grid { grid-template-columns: repeat(3, 1fr); }
    }
    @media (max-width: 820px) {
      main { width: min(100% - 28px, 620px); padding-top: 16px; }
      .topbar { align-items: start; flex-wrap: wrap; }
      .brand-subtitle, .status { display: none; }
      .hero, .content-grid { grid-template-columns: 1fr; }
      .power-panel, .creature-panel { min-height: 0; }
      .power-panel { min-height: 340px; }
      .stat-grid { grid-template-columns: repeat(2, 1fr); }
      .activity-panel { padding: 18px; }
    }
    @media (max-width: 500px) {
      main { width: min(100% - 20px, 420px); }
      .nav-link { padding: 6px 7px; font-size: 10px; }
      .hero { padding-top: 14px; }
      .creature-panel { padding: 16px; }
      .power-panel, .data-panel { padding: 18px; }
      .activity-panel { padding: 16px; }
      .power-value { font-size: clamp(30px, 10vw, 54px); }
      .activity-summary { align-items: start; flex-wrap: wrap; gap: 14px 20px; }
      .activity-window { width: 100%; margin-left: 0; }
      .activity-note { display: none; }
      .first-run { align-items: start; flex-direction: column; gap: 14px; }
      .command { width: 100%; overflow: auto; }
      footer { flex-direction: column; gap: 4px; }
    }
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
      <span class="brand-subtitle">your tokens are evolving</span>
    </a>
    <nav class="nav" aria-label="Primary navigation">
      <a class="nav-link active" href="/">Overview</a>
      <a class="nav-link" href="/api/v1/analytics/overview">Data</a>
      <span class="status" id="live-status"><span class="status-dot"></span><span id="live-status-text">local · connecting</span></span>
    </nav>
  </header>

  <section class="hero" aria-label="Current Tokemon and lifetime usage">
    <article class="panel creature-panel" data-stage="{{.Evolution.Stage}}">
      <div class="eyebrow">Current form <span>·</span> stage {{.Evolution.Stage}}</div>
      <div class="creature-art-wrap">
        <img class="creature-art" src="{{assetPath .Evolution.Stage}}" alt="{{.Evolution.FormName}}, stage {{.Evolution.Stage}}" onerror="this.hidden=true;this.nextElementSibling.hidden=false">
        <div class="creature-fallback" hidden>STAGE {{.Evolution.Stage}}</div>
      </div>
      <div class="creature-caption">
        <div><div class="eyebrow">Tokemon form</div><div class="form-name">{{.Evolution.FormName}}</div></div>
        <div class="stage-chip">FORM / {{printf "%02d" .Evolution.Stage}}</div>
      </div>
    </article>

    <article class="panel power-panel">
      <div class="power-header">
        <div>
          <div class="power-kicker">Lifetime tokens</div>
          <div class="power-value odometer" id="lifetime-counter" data-display="{{commas .LifetimeTokens}}" aria-label="{{commas .LifetimeTokens}}"><span class="odometer-static">{{commas .LifetimeTokens}}</span></div>
          <div class="power-unit">the power level behind this form</div>
          <div class="power-composition" id="token-composition" aria-label="Lifetime token composition">
            <span><strong>Input</strong> <span id="live-input-tokens">—</span></span>
            <span><strong>Uncached</strong> <span id="live-uncached-tokens">—</span></span>
            <span><strong>Cached</strong> <span id="live-cached-tokens">—</span></span>
            <span><strong>Output</strong> <span id="live-output-tokens">—</span></span>
            <span class="unclassified" id="live-unclassified" hidden><strong>Unclassified</strong> <span id="live-unclassified-tokens">—</span></span>
          </div>
        </div>
        <div class="power-note">
          <strong id="live-progress-label">{{printf "%.1f" (mul .Evolution.Progress 100)}}% charged</strong>
          <span id="live-remaining-copy">{{if .Evolution.TokensRemaining}}{{commasPtr .Evolution.TokensRemaining}} tokens until the next evolution{{else}}The Singularity has no next form.{{end}}</span>
        </div>
      </div>
      <div class="progress-block">
        <div class="progress-row"><span>Evolution progress</span><strong>Stage {{.Evolution.Stage}} → {{if .Evolution.NextThreshold}}{{commasPtr .Evolution.NextThreshold}}{{else}}∞{{end}}</strong></div>
        <div class="progress" id="live-progress" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow="{{percent .Evolution.Progress}}"><span></span></div>
        <div class="thresholds"><span>{{commas .Evolution.LowerThreshold}}</span><span>{{if .Evolution.NextThreshold}}{{commasPtr .Evolution.NextThreshold}}{{else}}FINAL FORM{{end}}</span></div>
      </div>
    </article>
  </section>

  <section class="stat-grid" aria-label="Usage summary">
    <article class="stat"><div class="stat-label">Power level</div><div class="stat-value" id="live-power-level">{{commas .LifetimeTokens}}</div><div class="stat-detail">lifetime tokens</div></article>
    <article class="stat"><div class="stat-label">Next evolution</div><div class="stat-value" id="live-next-evolution">{{if .Evolution.TokensRemaining}}{{commasPtr .Evolution.TokensRemaining}}{{else}}Final form{{end}}</div><div class="stat-detail">{{if .Evolution.NextThreshold}}tokens remaining{{else}}nothing beyond this{{end}}</div></article>
    <article class="stat"><div class="stat-label">Training ground</div>{{if .ByMachine}}<div class="stat-value">{{(index .ByMachine 0).Machine}}</div><div class="stat-detail">{{commas (index .ByMachine 0).Tokens}} tokens</div>{{else}}<div class="stat-value muted">Awaiting agent</div><div class="stat-detail">no machines yet</div>{{end}}</article>
    <article class="stat"><div class="stat-label">API-equivalent cost</div>{{if .EstimatedCost.PricedTokens}}<div class="stat-value">{{money .EstimatedCost.Amount}}</div><div class="stat-detail">Estimated from API pricing · {{coverage .EstimatedCost.PricedTokens .EstimatedCost.UnpricedTokens}}% coverage · not your bill</div>{{else}}<div class="stat-value muted">Unavailable</div><div class="stat-detail">no usage with known API pricing</div>{{end}}</article>
    <article class="stat"><div class="stat-label">Cache hit</div>{{if .Cache.EligibleTokens}}<div class="stat-value">{{printf "%.1f%%" (mul .Cache.HitRate 100)}}</div><div class="stat-detail">{{commas .Cache.CachedTokens}} cached of {{commas .Cache.EligibleTokens}} input tokens</div>{{else}}<div class="stat-value muted">Unavailable</div><div class="stat-detail">no classified input usage</div>{{end}}</article>
    <article class="stat"><div class="stat-label">Threads</div><div class="stat-value">{{commas .Threads}}</div><div class="stat-detail">distinct provider sessions</div></article>
  </section>

  <section class="panel activity-panel" aria-labelledby="activity-title">
    <div class="section-head">
      <div><div class="section-title" id="activity-title">Token activity</div><div class="section-subtitle">A pixel field of daily usage · hover any past day</div></div>
      <div class="section-mark">03 / FIELD LOG</div>
    </div>
    <div class="activity-summary">
      <div><strong>{{commas .Activity.TotalTokens}}</strong><span>tokens in field</span></div>
      <div><strong>{{commas .Activity.ActiveDays}}</strong><span>active days</span></div>
      <div class="activity-window">{{.Activity.StartDate}} → {{.Activity.EndDate}} · UTC</div>
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
    {{if .Activity.ActiveDays}}
    <div class="activity-legend"><span>LESS</span><span class="activity-cell level-0" aria-hidden="true"></span><span class="activity-cell level-1" aria-hidden="true"></span><span class="activity-cell level-2" aria-hidden="true"></span><span class="activity-cell level-3" aria-hidden="true"></span><span class="activity-cell level-4" aria-hidden="true"></span><span>MORE</span><span class="activity-note">Dashed edge = some token totals unavailable</span></div>
    {{else}}
    <div class="activity-empty">No token activity in this field yet. Your Tokemon is waiting for its first training session.</div>
    {{end}}
  </section>

  <section class="content-grid" aria-label="Usage breakdowns">
    <article class="panel data-panel">
      <div class="section-head"><div><div class="section-title">Usage by project</div><div class="section-subtitle">Merged across machines by project name</div></div><div class="section-mark">01 / PROJECTS</div></div>
      <table><thead><tr><th>Project</th><th>Machines</th><th>Tokens</th></tr></thead><tbody>{{range .ByProject}}<tr><td>{{.Project}}</td><td>{{commas .Machines}}</td><td>{{commas .Tokens}}</td></tr>{{else}}<tr class="empty-row"><td colspan="3">No privacy-safe project labels reported yet.</td></tr>{{end}}</tbody></table>
    </article>
    <article class="panel data-panel">
      <div class="section-head"><div><div class="section-title">Usage by model</div><div class="section-subtitle">Where the tokens are going</div></div><div class="section-mark">02 / MODELS</div></div>
      <table><thead><tr><th>Model</th><th>Tokens</th></tr></thead><tbody>{{range .ByModel}}<tr><td>{{.Model}}</td><td>{{commas .Tokens}}</td></tr>{{else}}<tr class="empty-row"><td colspan="2">No model usage yet. Your Tokemon is still waiting for its first token.</td></tr>{{end}}</tbody></table>
    </article>
    <article class="panel data-panel">
      <div class="section-head"><div><div class="section-title">Usage by machine</div><div class="section-subtitle">Your training grounds</div></div><div class="section-mark">03 / MACHINES</div></div>
      <table><thead><tr><th>Machine</th><th>Tokens</th></tr></thead><tbody>{{range .ByMachine}}<tr><td>{{.Machine}}</td><td>{{commas .Tokens}}</td></tr>{{else}}<tr class="empty-row"><td colspan="2">No machines have checked in yet.</td></tr>{{end}}</tbody></table>
    </article>
  </section>

  {{if eq .LifetimeTokens 0}}
  <section class="first-run" aria-label="Getting started">
    <div><h2>The egg is waiting for its first token.</h2><p>Run the agent on a machine with Claude Code, Codex, or a configured JSONL source. Tokemon only sends usage metadata.</p></div>
    <code class="command">tokemon agent --server …</code>
  </section>
  {{end}}

  <footer><span>SQLite · local-first · no conversation content</span><span>Just for fun · not affiliated with, endorsed by, or connected to Pokémon or The Pokémon Company.</span><span>live counter · analytics refresh every 60s · schema v1</span></footer>
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
      const size = Math.max(28, Math.min(86, width / Math.max(display.length * .68, 1)));
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

  document.querySelectorAll('.odometer').forEach((odometer) => {
    renderOdometer(odometer, odometer.dataset.display || '', true);
  });
  window.addEventListener('resize', () => {
    document.querySelectorAll('.odometer').forEach((odometer) => {
      const display = odometer.dataset.display || '';
      const width = odometer.getBoundingClientRect().width;
      odometer.style.fontSize = Math.max(28, Math.min(86, width / Math.max(display.length * .68, 1))) + 'px';
    });
  }, { passive: true });

  const number = new Intl.NumberFormat('en-US');
  const counter = document.getElementById('lifetime-counter');
  const status = document.getElementById('live-status');
  const statusText = document.getElementById('live-status-text');
  let lifetimeTokens = Number((counter.dataset.display || '0').replaceAll(',', ''));
  let currentStage = Number(document.querySelector('.creature-panel').dataset.stage);
  let polling = false;

  const setLiveStatus = (live) => {
    status.classList.toggle('is-live', live);
    status.classList.toggle('is-stale', !live);
    statusText.textContent = live ? 'local · live every 2s' : 'local · reconnecting';
  };

  const pollLifetime = async () => {
    if (polling || document.hidden) return;
    polling = true;
    try {
      const response = await fetch('/api/v1/evolution', { cache: 'no-store', headers: { Accept: 'application/json' } });
      if (!response.ok) throw new Error('live counter unavailable');
      const snapshot = await response.json();
      setLiveStatus(true);
      const composition = snapshot.composition || {};
      document.getElementById('live-input-tokens').textContent = number.format(composition.input_tokens || 0);
      document.getElementById('live-uncached-tokens').textContent = number.format(composition.uncached_input_tokens || 0);
      document.getElementById('live-cached-tokens').textContent = number.format(composition.cached_input_tokens || 0);
      document.getElementById('live-output-tokens').textContent = number.format(composition.output_tokens || 0);
      const unclassified = composition.unclassified_tokens || 0;
      document.getElementById('live-unclassified-tokens').textContent = number.format(unclassified);
      document.getElementById('live-unclassified').hidden = unclassified === 0;
      if (snapshot.stage !== currentStage) {
        location.reload();
        return;
      }
      if (snapshot.lifetime_tokens === lifetimeTokens) return;
      lifetimeTokens = snapshot.lifetime_tokens;
      const display = number.format(lifetimeTokens);
      renderOdometer(counter, display, true);
      document.getElementById('live-power-level').textContent = display;
      document.getElementById('live-progress-label').textContent = (snapshot.progress * 100).toFixed(1) + '% charged';
      document.getElementById('live-remaining-copy').textContent = snapshot.tokens_remaining == null
        ? 'The Singularity has no next form.'
        : number.format(snapshot.tokens_remaining) + ' tokens until the next evolution';
      document.getElementById('live-next-evolution').textContent = snapshot.tokens_remaining == null
        ? 'Final form'
        : number.format(snapshot.tokens_remaining);
      const progress = document.getElementById('live-progress');
      progress.setAttribute('aria-valuenow', String(snapshot.progress * 100));
      progress.firstElementChild.style.width = (snapshot.progress * 100) + '%';
    } catch (_) {
      setLiveStatus(false);
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

func twoDigits(value int) string {
	if value < 0 {
		value = 0
	}
	if value > 99 {
		value = 99
	}
	return strconv.FormatInt(int64(value/10), 10) + strconv.FormatInt(int64(value%10), 10)
}
