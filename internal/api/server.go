package api

import (
	"compress/gzip"
	"encoding/json"
	"html/template"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/usage"
)

type Server struct {
	store       *database.Store
	ingestToken string
	template    *template.Template
}

func New(store *database.Store, ingestToken string) (*Server, error) {
	page, err := template.New("dashboard").Funcs(template.FuncMap{
		"commas":    commas,
		"commasPtr": commasPtr,
		"mul":       func(left, right float64) float64 { return left * right },
		"percent":   func(value float64) float64 { return value * 100 },
	}).Parse(dashboardTemplate)
	if err != nil {
		return nil, err
	}
	return &Server{store: store, ingestToken: ingestToken, template: page}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
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
	writeJSON(w, http.StatusOK, result)
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
  <title>Tokemon</title>
  <style>
    :root { color-scheme: dark; --bg:#11110f; --surface:#181815; --text:#eee9df; --muted:#9f9a90; --border:#333129; --accent:#8aa18a; }
    * { box-sizing:border-box; } body { margin:0; background:var(--bg); color:var(--text); font:16px/1.5 system-ui,sans-serif; }
    main { width:min(1100px, calc(100% - 32px)); margin:0 auto; padding:32px 0 64px; }
    header { display:flex; justify-content:space-between; align-items:baseline; border-bottom:1px solid var(--border); padding-bottom:16px; }
    h1,h2,p { margin:0; } h1 { letter-spacing:.12em; font-size:18px; } h2 { font-size:13px; text-transform:uppercase; letter-spacing:.14em; color:var(--muted); }
    .hero { display:grid; grid-template-columns:1fr 1fr; gap:24px; padding:56px 0; border-bottom:1px solid var(--border); }
    .form { color:var(--accent); font-size:clamp(32px,6vw,72px); font-weight:700; letter-spacing:-.04em; }
    .tokens { font:clamp(36px,7vw,84px)/1.05 ui-monospace,SFMono-Regular,Menlo,monospace; overflow-wrap:anywhere; }
    .label { color:var(--muted); text-transform:uppercase; font-size:12px; letter-spacing:.12em; }
    .progress { height:8px; background:var(--border); margin-top:24px; } .progress span { display:block; height:100%; background:var(--accent); width:{{percent .Evolution.Progress}}%; }
    .meta { color:var(--muted); margin-top:12px; }
    .grid { display:grid; grid-template-columns:1fr 1fr; gap:24px; padding-top:32px; } section { border-top:1px solid var(--border); padding-top:16px; }
    table { width:100%; border-collapse:collapse; margin-top:12px; } th,td { text-align:left; padding:8px 0; border-bottom:1px solid var(--border); } th { color:var(--muted); font-size:12px; text-transform:uppercase; letter-spacing:.08em; } td:last-child,th:last-child { text-align:right; }
    @media (max-width:700px) { .hero,.grid { grid-template-columns:1fr; } .hero { padding:36px 0; } }
  </style>
</head>
<body><main>
  <header><h1>TOKEMON</h1><span class="label">Your tokens are evolving.</span></header>
  <div class="hero">
    <div><div class="label">Current form · stage {{.Evolution.Stage}}</div><div class="form">{{.Evolution.FormName}}</div><p class="meta">{{printf "%.1f" (mul .Evolution.Progress 100)}}% toward the next evolution</p><div class="progress"><span></span></div></div>
    <div><div class="label">Lifetime tokens</div><div class="tokens">{{commas .LifetimeTokens}}</div><p class="meta">{{if .Evolution.TokensRemaining}}{{commasPtr .Evolution.TokensRemaining}} tokens remaining{{else}}The Singularity has no next form.{{end}}</p></div>
  </div>
  <div class="grid">
    <section><h2>Usage by model</h2><table><tr><th>Model</th><th>Tokens</th></tr>{{range .ByModel}}<tr><td>{{.Model}}</td><td>{{commas .Tokens}}</td></tr>{{else}}<tr><td colspan="2" class="meta">No usage yet.</td></tr>{{end}}</table></section>
    <section><h2>Usage by machine</h2><table><tr><th>Machine</th><th>Tokens</th></tr>{{range .ByMachine}}<tr><td>{{.Machine}}</td><td>{{commas .Tokens}}</td></tr>{{else}}<tr><td colspan="2" class="meta">No machines have checked in.</td></tr>{{end}}</table></section>
  </div>
</main></body></html>`

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
