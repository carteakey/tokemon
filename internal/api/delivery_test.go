package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/usage"
)

func gunzipBytes(t *testing.T, compressed []byte) []byte {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("gzip.NewReader failed: %v", err)
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("gunzip ReadAll failed: %v", err)
	}
	return out
}

func TestGzipMiddlewareCompressesHTMLJSONCSVAndStaticJSON(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/gzip.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.Ingest(context.Background(), []usage.Event{{
		SchemaVersion: usage.SchemaVersion,
		EventID:       "gzip-ev-1",
		Timestamp:     time.Now().UTC(),
		MachineID:     "gzip-machine",
		Project:       "gzip-project",
		Provider:      "anthropic",
		Model:         "claude-sonnet-4",
		Tool:          "claude-code",
		SessionID:     "gzip-sess-1",
		TotalTokens:   usage.Int64(1000),
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "claude-code", AdapterVersion: "1"},
	}}); err != nil {
		t.Fatal(err)
	}

	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	handler := testHandler(server)

	routes := []struct {
		path     string
		wantCT   string
		wantBody string
	}{
		{path: "/", wantCT: "text/html", wantBody: "<!doctype html>"},
		{path: "/healthz", wantCT: "application/json", wantBody: `"status":"ok"`},
		{path: "/api/v1/machines", wantCT: "application/json", wantBody: `"gzip-machine"`},
		{path: "/api/v1/sessions/export?format=csv", wantCT: "text/csv", wantBody: "gzip-sess-1"},
		{path: "/api/v1/analytics/export?format=csv", wantCT: "text/csv", wantBody: "gzip-project"},
		{path: "/static/tokemon/manifest.json", wantCT: "application/json", wantBody: `"schema_version"`},
		{path: "/static/tokemon/does-not-exist.txt", wantCT: "text/plain", wantBody: "404 page not found"},
	}

	// Verify all additional compressible MIME types (CSS, JS, SVG) compress cleanly.
	mimeMux := http.NewServeMux()
	for _, tc := range []struct {
		path string
		ct   string
		body string
	}{
		{path: "/base.css", ct: "text/css; charset=utf-8", body: "body { color: #fff; }"},
		{path: "/app.js", ct: "application/javascript", body: "console.log('ok');"},
		{path: "/app-text.js", ct: "text/javascript; charset=utf-8", body: "console.log('ok');"},
		{path: "/card.svg", ct: "image/svg+xml", body: "<svg xmlns='http://www.w3.org/2000/svg'/>"},
	} {
		tc := tc
		mimeMux.HandleFunc("GET "+tc.path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", tc.ct)
			_, _ = w.Write([]byte(tc.body))
		})
	}
	mimeHandler := server.requestLogging(securityHeaders(gzipMiddleware(mimeMux)))
	for _, tc := range []struct {
		path string
		body string
	}{
		{path: "/base.css", body: "body { color: #fff; }"},
		{path: "/app.js", body: "console.log('ok');"},
		{path: "/app-text.js", body: "console.log('ok');"},
		{path: "/card.svg", body: "<svg xmlns='http://www.w3.org/2000/svg'/>"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		mimeHandler.ServeHTTP(rec, req)
		if rec.Header().Get("Content-Encoding") != "gzip" {
			t.Fatalf("%s Content-Encoding = %q, want gzip", tc.path, rec.Header().Get("Content-Encoding"))
		}
		if got := string(gunzipBytes(t, rec.Body.Bytes())); got != tc.body {
			t.Fatalf("%s gunzipped = %q, want %q", tc.path, got, tc.body)
		}
	}

	for _, tc := range routes {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Accept-Encoding", "gzip")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
				t.Fatalf("%s Content-Encoding = %q, want gzip", tc.path, got)
			}
			if got := rec.Header().Get("Content-Length"); got != "" {
				t.Fatalf("%s Content-Length = %q, want deleted when gzipped", tc.path, got)
			}
			if !strings.Contains(strings.ToLower(rec.Header().Get("Vary")), "accept-encoding") {
				t.Fatalf("%s Vary = %q, want Accept-Encoding", tc.path, rec.Header().Values("Vary"))
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, tc.wantCT) {
				t.Fatalf("%s Content-Type = %q, want substring %q", tc.path, ct, tc.wantCT)
			}
			plain := gunzipBytes(t, rec.Body.Bytes())
			if !strings.Contains(string(plain), tc.wantBody) {
				t.Fatalf("%s gunzipped body missing %q", tc.path, tc.wantBody)
			}

			// Verify uncompressed request matches gunzipped output byte-for-byte on deterministic routes.
			plainReq := httptest.NewRequest(http.MethodGet, tc.path, nil)
			plainRec := httptest.NewRecorder()
			handler.ServeHTTP(plainRec, plainReq)
			if plainRec.Header().Get("Content-Encoding") != "" {
				t.Fatalf("%s uncompressed Content-Encoding = %q", tc.path, plainRec.Header().Get("Content-Encoding"))
			}
			if !strings.Contains(strings.ToLower(plainRec.Header().Get("Vary")), "accept-encoding") {
				t.Fatalf("%s uncompressed Vary = %q, want Accept-Encoding", tc.path, plainRec.Header().Values("Vary"))
			}
			if !bytes.Equal(plain, plainRec.Body.Bytes()) {
				t.Fatalf("%s gunzipped body differs from uncompressed response", tc.path)
			}
		})
	}
}

func TestGzipMiddlewareLoginErrorWriteHeaderBeforeHTMLBody(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/login-gzip.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	server := passwordAccountServer(t, store)
	defer func() { _ = server.Close() }()

	// Fetch login form first to get a valid CSRF token and cookie.
	getReq := httptest.NewRequest(http.MethodGet, "/login", nil)
	getRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /login = %d", getRec.Code)
	}
	re := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)
	m := re.FindStringSubmatch(getRec.Body.String())
	if len(m) != 2 {
		t.Fatalf("missing csrf_token in /login HTML")
	}

	form := "username=accessibility-operator&password=wrong-password&csrf_token=" + m[1]
	postReq := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.Header.Set("Accept-Encoding", "gzip")
	postReq.Header.Set("Origin", "http://example.com")
	for _, c := range getRec.Result().Cookies() {
		postReq.AddCookie(c)
	}
	postRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(postRec, postReq)

	if postRec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /login wrong password = %d, want 401", postRec.Code)
	}
	if postRec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("POST /login Content-Encoding = %q, want gzip", postRec.Header().Get("Content-Encoding"))
	}
	if ct := postRec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("POST /login Content-Type = %q, want text/html", ct)
	}
	plain := string(gunzipBytes(t, postRec.Body.Bytes()))
	if !strings.Contains(plain, "Invalid username or password.") {
		t.Fatalf("POST /login gunzipped body missing error alert: %s", plain)
	}
}

func TestGzipMiddlewareSkipsBinaryNonGzipAndEdgeStatuses(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/gzip-skip.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	handler := testHandler(server)

	t.Run("skips PNG and WOFF2", func(t *testing.T) {
		for _, path := range []string{
			"/static/tokemon/stage-00.png",
			"/static/tokemon/fonts/pixelify-sans-latin.woff2",
		} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Accept-Encoding", "gzip")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s status = %d", path, rec.Code)
			}
			if got := rec.Header().Get("Content-Encoding"); got != "" {
				t.Fatalf("%s unexpectedly compressed: %q", path, got)
			}
			if rec.Header().Get("Content-Length") == "" {
				t.Fatalf("%s missing Content-Length", path)
			}
		}
	})

	t.Run("accept-encoding negotiation", func(t *testing.T) {
		cases := []struct {
			header     string
			wantGzip   bool
			wantVaryCt int
		}{
			{header: "", wantGzip: false},
			{header: "deflate, br", wantGzip: false},
			{header: "gzip;q=0", wantGzip: false},
			{header: "gzip;q=0.000", wantGzip: false},
			{header: "gzip;q=bad", wantGzip: false},
			{header: "gzip;q=1.5", wantGzip: false},
			{header: "x-gzip", wantGzip: true},
			{header: "deflate, gzip;q=0.8", wantGzip: true},
		}
		for _, tc := range cases {
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			if tc.header != "" {
				req.Header.Set("Accept-Encoding", tc.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			gotGzip := rec.Header().Get("Content-Encoding") == "gzip"
			if gotGzip != tc.wantGzip {
				t.Fatalf("Accept-Encoding=%q compressed=%v, want %v", tc.header, gotGzip, tc.wantGzip)
			}
			if tc.wantGzip {
				_ = gunzipBytes(t, rec.Body.Bytes())
			}
		}
	})

	t.Run("skips HEAD, 204, 304, 206, empty body, no-transform, pre-encoded, Upgrade, and deduplicates Vary", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /204", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("GET /304", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotModified)
		})
		mux.HandleFunc("GET /206", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("partial"))
		})
		mux.HandleFunc("GET /empty", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
		})
		mux.HandleFunc("GET /no-transform", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Cache-Control", "public, no-transform")
			_, _ = w.Write([]byte("raw text"))
		})
		mux.HandleFunc("GET /pre-encoded", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Encoding", "br")
			_, _ = w.Write([]byte("br-bytes"))
		})
		mux.HandleFunc("GET /with-vary", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Add("Vary", "Origin, Accept-Encoding")
			_, _ = w.Write([]byte("hello vary"))
		})
		mux.HandleFunc("HEAD /head-text", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Length", "5")
			w.WriteHeader(http.StatusOK)
		})

		h := server.requestLogging(securityHeaders(gzipMiddleware(mux)))

		for _, path := range []string{"/204", "/304", "/206", "/empty", "/no-transform", "/pre-encoded"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Accept-Encoding", "gzip")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Header().Get("Content-Encoding") == "gzip" {
				t.Fatalf("%s unexpectedly gzipped", path)
			}
			if path == "/empty" && rec.Body.Len() != 0 {
				t.Fatalf("/empty wrote %d bytes, want 0", rec.Body.Len())
			}
		}

		headReq := httptest.NewRequest(http.MethodHead, "/head-text", nil)
		headReq.Header.Set("Accept-Encoding", "gzip")
		headRec := httptest.NewRecorder()
		h.ServeHTTP(headRec, headReq)
		if headRec.Header().Get("Content-Encoding") != "" {
			t.Fatalf("HEAD Content-Encoding = %q", headRec.Header().Get("Content-Encoding"))
		}
		if headRec.Header().Get("Content-Length") != "5" {
			t.Fatalf("HEAD Content-Length = %q, want 5", headRec.Header().Get("Content-Length"))
		}
		if !strings.Contains(strings.ToLower(headRec.Header().Get("Vary")), "accept-encoding") {
			t.Fatalf("HEAD missing Vary: Accept-Encoding")
		}

		varyReq := httptest.NewRequest(http.MethodGet, "/with-vary", nil)
		varyReq.Header.Set("Accept-Encoding", "gzip")
		varyRec := httptest.NewRecorder()
		h.ServeHTTP(varyRec, varyReq)
		if len(varyRec.Header().Values("Vary")) != 1 {
			t.Fatalf("Vary duplicated: %v", varyRec.Header().Values("Vary"))
		}

		upgReq := httptest.NewRequest(http.MethodGet, "/with-vary", nil)
		upgReq.Header.Set("Accept-Encoding", "gzip")
		upgReq.Header.Set("Upgrade", "websocket")
		upgRec := httptest.NewRecorder()
		h.ServeHTTP(upgRec, upgReq)
		if upgRec.Header().Get("Content-Encoding") != "" {
			t.Fatalf("Upgrade request unexpectedly compressed")
		}
	})
}

type deadlineTrackingResponseWriter struct {
	*httptest.ResponseRecorder
	deadlineSet time.Time
	flushed     int
}

func (w *deadlineTrackingResponseWriter) SetWriteDeadline(t time.Time) error {
	w.deadlineSet = t
	return nil
}

func (w *deadlineTrackingResponseWriter) Flush() {
	w.flushed++
	w.ResponseRecorder.Flush()
}

func TestGzipMiddlewarePreservesResponseControllerDeadlineAndFlush(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/rc.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()

	wantDeadline := time.Now().Add(5 * time.Second).Truncate(time.Second)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stream", func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		if err := rc.SetWriteDeadline(wantDeadline); err != nil {
			t.Fatalf("SetWriteDeadline failed: %v", err)
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("col1,col2\n"))
		if err := rc.Flush(); err != nil {
			t.Fatalf("Flush failed: %v", err)
		}
		_, _ = w.Write([]byte("a,b\n"))
	})

	h := server.requestLogging(securityHeaders(gzipMiddleware(mux)))
	baseRec := &deadlineTrackingResponseWriter{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(baseRec, req)

	if !baseRec.deadlineSet.Equal(wantDeadline) {
		t.Fatalf("deadline = %v, want %v", baseRec.deadlineSet, wantDeadline)
	}
	if baseRec.flushed == 0 {
		t.Fatal("expected underlying ResponseWriter Flush() to be called")
	}
	if baseRec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", baseRec.Header().Get("Content-Encoding"))
	}
	plain := string(gunzipBytes(t, baseRec.Body.Bytes()))
	if plain != "col1,col2\na,b\n" {
		t.Fatalf("gunzipped stream = %q, want col1,col2\\na,b\\n", plain)
	}
}

func TestStaticAssetVersioningDeterminismAndCacheHeaders(t *testing.T) {
	fs1 := fstest.MapFS{
		"a.png": {Data: []byte("png-bytes")},
		"b.css": {Data: []byte("body{}")},
	}
	fs2 := fstest.MapFS{
		"a.png": {Data: []byte("png-bytes")},
		"b.css": {Data: []byte("body{}")},
	}
	fsBytesChanged := fstest.MapFS{
		"a.png": {Data: []byte("png-bytes-2")},
		"b.css": {Data: []byte("body{}")},
	}
	fsNameChanged := fstest.MapFS{
		"a.png":  {Data: []byte("png-bytes")},
		"b2.css": {Data: []byte("body{}")},
	}

	v1, err := computeStaticAssetVersion(fs1)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := computeStaticAssetVersion(fs2)
	if err != nil {
		t.Fatal(err)
	}
	if len(v1) != 12 || v1 != v2 {
		t.Fatalf("version determinism failed: v1=%q v2=%q", v1, v2)
	}
	vBytes, err := computeStaticAssetVersion(fsBytesChanged)
	if err != nil || vBytes == v1 {
		t.Fatalf("version did not change on content mutation: v1=%q vBytes=%q err=%v", v1, vBytes, err)
	}
	vName, err := computeStaticAssetVersion(fsNameChanged)
	if err != nil || vName == v1 {
		t.Fatalf("version did not change on filename mutation: v1=%q vName=%q err=%v", v1, vName, err)
	}

	store, err := database.Open(t.TempDir()+"/static.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	handler := testHandler(server)

	for _, path := range []string{
		"/static/tokemon/stage-00.png?v=" + server.assetVersion,
		"/static/tokemon/fonts/pixelify-sans-latin.woff2?v=" + server.assetVersion,
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != staticImmutableCacheControl {
			t.Fatalf("%s Cache-Control = %q, want %q", path, got, staticImmutableCacheControl)
		}
	}

	missingReq := httptest.NewRequest(http.MethodGet, "/static/tokemon/stage-99.png", nil)
	missingRec := httptest.NewRecorder()
	handler.ServeHTTP(missingRec, missingReq)
	if missingRec.Code != http.StatusNotFound {
		t.Fatalf("missing static status = %d, want 404", missingRec.Code)
	}
	if strings.Contains(missingRec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("404 static response unexpectedly marked immutable: %q", missingRec.Header().Get("Cache-Control"))
	}

	dirReq := httptest.NewRequest(http.MethodGet, "/static/tokemon/", nil)
	dirRec := httptest.NewRecorder()
	handler.ServeHTTP(dirRec, dirReq)
	if strings.Contains(dirRec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("directory static response unexpectedly marked immutable: %q", dirRec.Header().Get("Cache-Control"))
	}
}

func TestAllSevenTemplatesServeVersionedStaticURLsFontPreloadLCPAndContentVisibility(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/templates-delivery.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	server := passwordAccountServer(t, store)
	defer func() { _ = server.Close() }()

	wantFontURL := "/static/tokemon/fonts/pixelify-sans-latin.woff2?v=" + server.assetVersion
	wantPreload := `<link rel="preload" as="font" type="font/woff2" href="` + wantFontURL + `" crossorigin>`
	wantFontFace := `url("` + wantFontURL + `") format("woff2")`
	staticRefPattern := regexp.MustCompile(`/static/[^\s"')]+`)

	for _, path := range []string{"/", "/analytics", "/sessions", "/settings", "/insights", "/tokedex", "/login"} {
		body := renderAccessiblePage(t, server, path)
		if strings.Contains(body, "ZgotmplZ") {
			t.Fatalf("%s contains ZgotmplZ template escaping marker", path)
		}
		if !strings.Contains(body, wantPreload) {
			t.Fatalf("%s missing font preload link %q", path, wantPreload)
		}
		if !strings.Contains(body, wantFontFace) {
			t.Fatalf("%s @font-face src does not match preload URL %q", path, wantFontFace)
		}
		refs := staticRefPattern.FindAllString(body, -1)
		if len(refs) == 0 {
			t.Fatalf("%s rendered zero /static/ references", path)
		}
		for _, ref := range refs {
			if !strings.Contains(ref, "?v="+server.assetVersion) {
				t.Fatalf("%s contains unversioned static reference %q (want ?v=%s)", path, ref, server.assetVersion)
			}
		}
	}

	dashboardBody := renderAccessiblePage(t, server, "/")
	for _, want := range []string{
		`width: auto; max-width: min(100%, 278px); height: auto; max-height: 252px; object-fit: contain;`,
		`.health-panel, .sessions-panel { padding: 12px 16px; content-visibility: auto; contain-intrinsic-size: auto none auto 320px; }`,
		`fetchpriority="high"`,
	} {
		if !strings.Contains(dashboardBody, want) {
			t.Fatalf("/ missing LCP/content-visibility contract %q", want)
		}
	}

	analyticsBody := renderAccessiblePage(t, server, "/analytics")
	for _, want := range []string{
		`.share-panel { position: relative; min-width: 0; margin-top: 10px; overflow: hidden; content-visibility: auto; contain-intrinsic-size: auto none auto 320px; }`,
		`.sessions-panel { margin-top: 10px; content-visibility: auto; contain-intrinsic-size: auto none auto 320px; }`,
	} {
		if !strings.Contains(analyticsBody, want) {
			t.Fatalf("/analytics missing content-visibility contract %q", want)
		}
	}

	sessionsBody := renderAccessiblePage(t, server, "/sessions")
	if strings.Contains(sessionsBody, "content-visibility") {
		t.Fatalf("/sessions .session-panel is above the fold and must not set content-visibility")
	}
}
