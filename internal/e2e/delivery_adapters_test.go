//go:build e2e

package e2e_test

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func noAutoDecompressClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DisableCompression: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func doRawHTTP(t *testing.T, client *http.Client, method, fullURL, bearer string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, fullURL, nil)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, fullURL, err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, fullURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body %s %s: %v", method, fullURL, err)
	}
	return resp, body
}

func gunzipBytes(t *testing.T, compressed []byte) []byte {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	return out
}

func varyHasAcceptEncoding(resp *http.Response) bool {
	for _, v := range resp.Header.Values("Vary") {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), "Accept-Encoding") {
				return true
			}
		}
	}
	return false
}

// --- Area D: HTTP Delivery & Static Assets (Feature 11) ---

func TestDelivery_GzipCompressesHTMLJSONSVGCSV(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "deliv-gzip-tok"})
	baseTime := time.Now().UTC().Add(-2 * time.Hour)
	events := make([]ingestEvent, 0, 12)
	for i := 0; i < 12; i++ {
		events = append(events, ingestEvent{
			EventID:      fmt.Sprintf("deliv-gzip-%d", i+1),
			Timestamp:    baseTime.Add(time.Duration(i) * time.Minute).Format(time.RFC3339),
			MachineID:    "m-deliv",
			SessionID:    fmt.Sprintf("s-deliv-%d", i+1),
			Project:      fmt.Sprintf("proj-deliv-%02d", i+1),
			Provider:     "openai",
			Model:        "gpt-5.4",
			Tool:         "codex",
			InputTokens:  i64(100),
			OutputTokens: i64(50),
			TotalTokens:  i64(150),
		})
	}
	s.ingestEvents(t, events)

	client := noAutoDecompressClient()
	// M2 Feature 11 core endpoints (M3 base.css & M5 card.svg are verified in TestDelivery_CardSVGAndBaseCSSGzipAndCache).
	targets := []struct {
		path   string
		verify func(t *testing.T, plain []byte)
	}{
		{
			path: "/",
			verify: func(t *testing.T, plain []byte) {
				if !strings.Contains(string(plain), "<html") {
					t.Fatalf("expected HTML from /")
				}
			},
		},
		{
			path: "/analytics",
			verify: func(t *testing.T, plain []byte) {
				if !strings.Contains(string(plain), "Analytics") {
					t.Fatalf("expected Analytics HTML")
				}
			},
		},
		{
			path: "/api/v1/evolution",
			verify: func(t *testing.T, plain []byte) {
				var v map[string]any
				if err := json.Unmarshal(plain, &v); err != nil {
					t.Fatalf("invalid evolution JSON: %v", err)
				}
			},
		},
		{
			path: "/api/v1/analytics",
			verify: func(t *testing.T, plain []byte) {
				var v map[string]any
				if err := json.Unmarshal(plain, &v); err != nil {
					t.Fatalf("invalid analytics JSON: %v", err)
				}
			},
		},
		{
			path: "/api/v1/analytics/export?format=csv",
			verify: func(t *testing.T, plain []byte) {
				recs, err := csv.NewReader(bytes.NewReader(plain)).ReadAll()
				if err != nil || len(recs) < 11 {
					t.Fatalf("expected >=10 data rows (+header) in CSV export: err=%v rows=%d", err, len(recs))
				}
			},
		},
	}

	for _, tc := range targets {
		t.Run(tc.path, func(t *testing.T) {
			resp, raw := doRawHTTP(t, client, http.MethodGet, s.BaseURL+tc.path, s.DashboardToken, map[string]string{
				"Accept-Encoding": "gzip",
			})
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s status = %d, want 200: %s", tc.path, resp.StatusCode, string(raw))
			}
			if ce := resp.Header.Get("Content-Encoding"); ce != "gzip" {
				t.Fatalf("GET %s Content-Encoding = %q, want gzip", tc.path, ce)
			}
			if !varyHasAcceptEncoding(resp) {
				t.Fatalf("GET %s missing Vary: Accept-Encoding (got %v)", tc.path, resp.Header.Values("Vary"))
			}
			plain := gunzipBytes(t, raw)
			tc.verify(t, plain)
		})
	}
}

func TestDelivery_StaticAssetsImmutableCacheAndVersionedURLs(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "deliv-cache-tok"})
	auth := s.authClient(t)
	resp, err := auth.Get(s.BaseURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	htmlBytes, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read /: %v", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d", resp.StatusCode)
	}
	html := string(htmlBytes)

	patterns := []string{
		`/static/tokemon/token-dex\.png\?v=[^"'\s>]+`,
		`/static/tokemon/stage-\d+\.png\?v=[^"'\s>]+`,
		`/static/tokemon/fonts/pixelify-sans-latin\.woff2\?v=[^"'\s>]+`,
	}
	for _, pat := range patterns {
		re := regexp.MustCompile(pat)
		assetURL := re.FindString(html)
		if assetURL == "" {
			t.Fatalf("GET / HTML missing versioned static URL matching %s", pat)
		}
		aResp, err := auth.Get(s.BaseURL + assetURL)
		if err != nil {
			t.Fatalf("GET %s: %v", assetURL, err)
		}
		_, _ = io.Copy(io.Discard, aResp.Body)
		_ = aResp.Body.Close()
		if aResp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", assetURL, aResp.StatusCode)
		}
		if cc := aResp.Header.Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
			t.Fatalf("GET %s Cache-Control = %q, want public, max-age=31536000, immutable", assetURL, cc)
		}
	}
}

var (
	preloadLinkRe       = regexp.MustCompile(`(?i)<link\b[^>]*\brel="preload"[^>]*>`)
	creatureImgRe       = regexp.MustCompile(`<img\b[^>]*class="[^"]*\bcreature-art\b[^"]*"[^>]*>`)
	widthAttrRe         = regexp.MustCompile(`\bwidth="\d+"`)
	heightAttrRe        = regexp.MustCompile(`\bheight="\d+"`)
	baseCSSHrefRe       = regexp.MustCompile(`/static/tokemon/base\.css(?:\?v=[^"'\s>]+)?`)
	contentVisibilityRe = regexp.MustCompile(`content-visibility\s*:\s*auto`)
	containIntrinsicRe  = regexp.MustCompile(`contain-intrinsic-size\s*:`)
)

func TestDelivery_LCPPreloadDimensionsAndContentVisibility(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "deliv-lcp-tok"})
	auth := s.authClient(t)

	resp, err := auth.Get(s.BaseURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	htmlBytes, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read /: %v", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d", resp.StatusCode)
	}
	html := string(htmlBytes)

	var hasFontPreload bool
	for _, tag := range preloadLinkRe.FindAllString(html, -1) {
		if strings.Contains(tag, `as="font"`) &&
			strings.Contains(tag, `pixelify-sans-latin.woff2`) &&
			strings.Contains(tag, `crossorigin`) {
			hasFontPreload = true
			break
		}
	}
	if !hasFontPreload {
		t.Fatalf("GET / missing <link rel=\"preload\" ... as=\"font\" ... pixelify-sans-latin.woff2 ... crossorigin>")
	}
	imgTag := creatureImgRe.FindString(html)
	if imgTag == "" {
		t.Fatalf("GET / missing <img class=\"creature-art\">")
	}
	if !strings.Contains(imgTag, `fetchpriority="high"`) {
		t.Fatalf(".creature-art missing fetchpriority=\"high\": %s", imgTag)
	}
	if !widthAttrRe.MatchString(imgTag) || !heightAttrRe.MatchString(imgTag) {
		t.Fatalf(".creature-art missing explicit width/height attributes: %s", imgTag)
	}

	cssCorpus := html
	if cssHref := baseCSSHrefRe.FindString(html); cssHref != "" {
		cResp, err := auth.Get(s.BaseURL + cssHref)
		if err != nil {
			t.Fatalf("GET %s: %v", cssHref, err)
		}
		cssBytes, cssErr := io.ReadAll(cResp.Body)
		_ = cResp.Body.Close()
		if cssErr != nil {
			t.Fatalf("read %s: %v", cssHref, cssErr)
		}
		if cResp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", cssHref, cResp.StatusCode)
		}
		cssCorpus += "\n" + string(cssBytes)
	}
	if !contentVisibilityRe.MatchString(cssCorpus) || !containIntrinsicRe.MatchString(cssCorpus) {
		t.Fatalf("CSS missing content-visibility: auto or contain-intrinsic-size for below-the-fold panels")
	}
}

func TestDelivery_UncompressedClientsReceivePlaintextWithVary(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "deliv-plain-tok"})
	client := noAutoDecompressClient()

	for _, path := range []string{"/", "/api/v1/evolution", "/api/v1/analytics"} {
		resp, body := doRawHTTP(t, client, http.MethodGet, s.BaseURL+path, s.DashboardToken, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("plain GET %s status = %d, want 200", path, resp.StatusCode)
		}
		if ce := resp.Header.Get("Content-Encoding"); ce != "" {
			t.Fatalf("plain GET %s unexpectedly compressed with %q", path, ce)
		}
		if !varyHasAcceptEncoding(resp) {
			t.Fatalf("plain GET %s missing Vary: Accept-Encoding", path)
		}
		if len(body) == 0 {
			t.Fatalf("plain GET %s returned empty body", path)
		}
	}
}

func TestDelivery_PNGAssetsRemainValid8BitRGBAAndCacheable(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "deliv-png-tok"})
	auth := s.authClient(t)

	for _, tc := range []struct {
		path            string
		require8BitRGBA bool
	}{
		{path: "/static/tokemon/stage-00.png", require8BitRGBA: true},
		{path: "/static/tokemon/stage-09.png", require8BitRGBA: true},
		{path: "/static/tokemon/token-dex.png", require8BitRGBA: false},
	} {
		resp, err := auth.Get(s.BaseURL + tc.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		raw, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", tc.path, readErr)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d", tc.path, resp.StatusCode)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
			t.Fatalf("GET %s Cache-Control = %q", tc.path, cc)
		}
		if !bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")) {
			t.Fatalf("PNG %s missing PNG magic bytes", tc.path)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("png.DecodeConfig(%s): %v", tc.path, err)
		}
		if cfg.Width <= 0 || cfg.Height <= 0 {
			t.Fatalf("PNG %s invalid dims %dx%d", tc.path, cfg.Width, cfg.Height)
		}
		if tc.require8BitRGBA {
			// Verify PNG IHDR explicitly reports bit depth == 8, color type == 6 (RGBA) for manifest stage assets.
			if len(raw) < 26 || string(raw[12:16]) != "IHDR" || binary.BigEndian.Uint32(raw[8:12]) != 13 {
				t.Fatalf("PNG %s malformed IHDR", tc.path)
			}
			if raw[24] != 8 || raw[25] != 6 {
				t.Fatalf("PNG %s bitDepth=%d colorType=%d, want 8-bit RGBA (8, 6)", tc.path, raw[24], raw[25])
			}
		}
	}
}

func TestDelivery_GzipSkipsBinaryPNGAndWoff2(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "deliv-nogz-bin"})
	client := noAutoDecompressClient()

	cases := []struct {
		path  string
		magic []byte
	}{
		{"/static/tokemon/stage-00.png", []byte("\x89PNG\r\n\x1a\n")},
		{"/static/tokemon/token-dex.png", []byte("\x89PNG\r\n\x1a\n")},
		{"/static/tokemon/fonts/pixelify-sans-latin.woff2", []byte("wOF2")},
	}
	for _, tc := range cases {
		resp, raw := doRawHTTP(t, client, http.MethodGet, s.BaseURL+tc.path, "", map[string]string{
			"Accept-Encoding": "gzip",
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d", tc.path, resp.StatusCode)
		}
		if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
			t.Fatalf("binary asset %s must not be gzip-compressed", tc.path)
		}
		if !bytes.HasPrefix(raw, tc.magic) {
			t.Fatalf("asset %s missing expected magic prefix %x (got %x)", tc.path, tc.magic, raw[:min(len(raw), 8)])
		}
	}
}

func TestDelivery_NonGzipAcceptEncodingNotCompressed(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "deliv-nogz-hdr"})
	client := noAutoDecompressClient()

	for _, ae := range []string{"br, deflate", "identity"} {
		for _, path := range []string{"/", "/api/v1/evolution"} {
			resp, raw := doRawHTTP(t, client, http.MethodGet, s.BaseURL+path, s.DashboardToken, map[string]string{
				"Accept-Encoding": ae,
			})
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s (AE=%q) status = %d", path, ae, resp.StatusCode)
			}
			if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
				t.Fatalf("GET %s with Accept-Encoding=%q unexpectedly returned gzip", path, ae)
			}
			if len(raw) == 0 {
				t.Fatalf("GET %s empty body", path)
			}
		}
	}
}

func TestDelivery_AuthenticatedPagesStayNoStoreWhileStaticIsImmutable(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "deliv-nostore-tok"})
	auth := s.authClient(t)

	for _, path := range []string{"/", "/analytics", "/api/v1/evolution", "/api/v1/analytics"} {
		resp, err := auth.Get(s.BaseURL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d", path, resp.StatusCode)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("authenticated %s Cache-Control = %q, want no-store", path, cc)
		}
	}

	sResp, err := auth.Get(s.BaseURL + "/static/tokemon/stage-00.png")
	if err != nil {
		t.Fatalf("GET /static/tokemon/stage-00.png: %v", err)
	}
	_, _ = io.Copy(io.Discard, sResp.Body)
	_ = sResp.Body.Close()
	if sResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /static/tokemon/stage-00.png status = %d", sResp.StatusCode)
	}
	if cc := sResp.Header.Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Fatalf("static Cache-Control = %q, want public, max-age=31536000, immutable", cc)
	}
}

func TestDelivery_CardSVGAndBaseCSSGzipAndCache(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "deliv-m3m5-tok"})
	auth := s.authClient(t)
	client := noAutoDecompressClient()

	// M3: /static/tokemon/base.css versioned URL on /, immutable Cache-Control, gzip + Vary, plaintext + Vary.
	homeResp, err := auth.Get(s.BaseURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	homeHTML, readErr := io.ReadAll(homeResp.Body)
	_ = homeResp.Body.Close()
	if readErr != nil || homeResp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status=%d err=%v", homeResp.StatusCode, readErr)
	}
	baseCSSVersionedRe := regexp.MustCompile(`/static/tokemon/base\.css\?v=[^"'\s>]+`)
	cssURL := baseCSSVersionedRe.FindString(string(homeHTML))
	if cssURL == "" {
		t.Fatalf("GET / HTML missing versioned /static/tokemon/base.css?v=... link")
	}

	cssGzResp, cssGzRaw := doRawHTTP(t, client, http.MethodGet, s.BaseURL+cssURL, s.DashboardToken, map[string]string{
		"Accept-Encoding": "gzip",
	})
	if cssGzResp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (gzip) status = %d", cssURL, cssGzResp.StatusCode)
	}
	if cc := cssGzResp.Header.Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Fatalf("GET %s Cache-Control = %q, want public, max-age=31536000, immutable", cssURL, cc)
	}
	if ce := cssGzResp.Header.Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("GET %s Content-Encoding = %q, want gzip", cssURL, ce)
	}
	if !varyHasAcceptEncoding(cssGzResp) {
		t.Fatalf("GET %s missing Vary: Accept-Encoding", cssURL)
	}
	cssPlain := gunzipBytes(t, cssGzRaw)
	if len(cssPlain) == 0 || !strings.Contains(string(cssPlain), "{") {
		t.Fatalf("expected valid CSS content from %s", cssURL)
	}

	cssPlainResp, cssPlainBody := doRawHTTP(t, client, http.MethodGet, s.BaseURL+"/static/tokemon/base.css", s.DashboardToken, nil)
	if cssPlainResp.StatusCode != http.StatusOK || cssPlainResp.Header.Get("Content-Encoding") != "" || !varyHasAcceptEncoding(cssPlainResp) || len(cssPlainBody) == 0 {
		t.Fatalf("plain GET /static/tokemon/base.css unexpected headers/body: status=%d ce=%q vary=%v len=%d",
			cssPlainResp.StatusCode, cssPlainResp.Header.Get("Content-Encoding"), cssPlainResp.Header.Values("Vary"), len(cssPlainBody))
	}

	// M5: /api/v1/card.svg gzip + Vary, plaintext + Vary, authenticated Cache-Control: no-store.
	svgGzResp, svgGzRaw := doRawHTTP(t, client, http.MethodGet, s.BaseURL+"/api/v1/card.svg", s.DashboardToken, map[string]string{
		"Accept-Encoding": "gzip",
	})
	if svgGzResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/card.svg (gzip) status = %d", svgGzResp.StatusCode)
	}
	if cc := svgGzResp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("GET /api/v1/card.svg Cache-Control = %q, want no-store", cc)
	}
	if ce := svgGzResp.Header.Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("GET /api/v1/card.svg Content-Encoding = %q, want gzip", ce)
	}
	if !varyHasAcceptEncoding(svgGzResp) {
		t.Fatalf("GET /api/v1/card.svg missing Vary: Accept-Encoding")
	}
	svgPlain := gunzipBytes(t, svgGzRaw)
	if !strings.Contains(string(svgPlain), "<svg") {
		t.Fatalf("expected SVG payload from /api/v1/card.svg")
	}

	svgPlainResp, svgPlainBody := doRawHTTP(t, client, http.MethodGet, s.BaseURL+"/api/v1/card.svg", s.DashboardToken, nil)
	if svgPlainResp.StatusCode != http.StatusOK || svgPlainResp.Header.Get("Content-Encoding") != "" || !varyHasAcceptEncoding(svgPlainResp) || !strings.Contains(string(svgPlainBody), "<svg") {
		t.Fatalf("plain GET /api/v1/card.svg unexpected response: status=%d ce=%q vary=%v",
			svgPlainResp.StatusCode, svgPlainResp.Header.Get("Content-Encoding"), svgPlainResp.Header.Values("Vary"))
	}
}

func TestDelivery_MissingStaticAndCacheBustedQueryFallback(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "deliv-static-fb"})
	resp404, _ := s.doReq(t, http.MethodGet, "/static/tokemon/stage-99.png", nil, nil)
	if resp404.StatusCode != http.StatusNotFound {
		t.Fatalf("GET missing static status = %d, want 404", resp404.StatusCode)
	}

	respOK, raw := s.doReq(t, http.MethodGet, "/static/tokemon/stage-00.png?v=arbitrary-mismatch", nil, nil)
	if respOK.StatusCode != http.StatusOK {
		t.Fatalf("GET stage-00 with ?v=mismatch status = %d, want 200", respOK.StatusCode)
	}
	if !bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatalf("expected valid PNG for cache-busted static request")
	}
}

func TestDelivery_HeadAndEmptyBodyNotBrokenByGzip(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "deliv-head-tok"})
	client := noAutoDecompressClient()

	for _, path := range []string{"/", "/api/v1/evolution"} {
		resp, body := doRawHTTP(t, client, http.MethodHead, s.BaseURL+path, s.DashboardToken, map[string]string{
			"Accept-Encoding": "gzip",
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("HEAD %s status = %d, want 200", path, resp.StatusCode)
		}
		if len(body) != 0 {
			t.Fatalf("HEAD %s returned non-empty body (%d bytes)", path, len(body))
		}
	}
}

// --- Area E: Incremental Facets (Feature 7) & /api/v1/evolution SWR (Feature 10-evo) ---

type analyticsFacetsPayload struct {
	Facets struct {
		Machines  []string `json:"machines"`
		Providers []string `json:"providers"`
		Models    []string `json:"models"`
		Tools     []string `json:"tools"`
		Projects  []string `json:"projects"`
	} `json:"facets"`
}

type evolutionPayload struct {
	LifetimeTokens  int64   `json:"lifetime_tokens"`
	Stage           int     `json:"stage"`
	Form            string  `json:"form"`
	FormName        string  `json:"form_name"`
	LowerThreshold  int64   `json:"lower_threshold"`
	NextThreshold   *int64  `json:"next_threshold"`
	TokensRemaining *int64  `json:"tokens_remaining"`
	Progress        float64 `json:"progress"`
	Composition     struct {
		InputTokens         int64 `json:"input_tokens"`
		UncachedInputTokens int64 `json:"uncached_input_tokens"`
		CachedInputTokens   int64 `json:"cached_input_tokens"`
		OutputTokens        int64 `json:"output_tokens"`
		UnclassifiedTokens  int64 `json:"unclassified_tokens"`
	} `json:"composition"`
}

func containsAll(slice []string, wants ...string) bool {
	set := make(map[string]bool, len(slice))
	for _, s := range slice {
		set[s] = true
	}
	for _, w := range wants {
		if !set[w] {
			return false
		}
	}
	return true
}

func TestFacets_NewIngestImmediatelyVisibleInAPIAndSessionsHTML(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "facets-imm-tok"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "fac-seed-1",
		MachineID:   "machine-a",
		SessionID:   "sess-a",
		Project:     "project-alpha",
		Provider:    "provider-a",
		Model:       "model-a",
		Tool:        "tool-a",
		TotalTokens: i64(50),
	}})
	auth := s.authClient(t)
	// Warm /sessions and default /api/v1/analytics within 60s TTL.
	for _, warmPath := range []string{"/sessions", "/api/v1/analytics"} {
		warmResp, err := auth.Get(s.BaseURL + warmPath)
		if err != nil {
			t.Fatalf("warm GET %s: %v", warmPath, err)
		}
		_, _ = io.Copy(io.Discard, warmResp.Body)
		_ = warmResp.Body.Close()
		if warmResp.StatusCode != http.StatusOK {
			t.Fatalf("warm GET %s status = %d, want 200", warmPath, warmResp.StatusCode)
		}
	}

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "fac-new-2",
		MachineID:   "machine-b",
		SessionID:   "sess-b",
		Project:     "project-beta",
		Provider:    "provider-b",
		Model:       "model-b",
		Tool:        "tool-b",
		TotalTokens: i64(75),
	}})

	// Immediately query cold-key /api/v1/analytics and /sessions HTML without waiting 60s TTL.
	var ana analyticsFacetsPayload
	getJSONWithClient(t, auth, s.BaseURL+"/api/v1/analytics?period=7d&machine=machine-b", &ana)
	if !containsAll(ana.Facets.Machines, "machine-a", "machine-b") ||
		!containsAll(ana.Facets.Providers, "provider-a", "provider-b") ||
		!containsAll(ana.Facets.Models, "model-a", "model-b") ||
		!containsAll(ana.Facets.Tools, "tool-a", "tool-b") ||
		!containsAll(ana.Facets.Projects, "project-alpha", "project-beta") {
		t.Fatalf("incremental facets missing newly ingested values: %+v", ana.Facets)
	}

	sessResp, err := auth.Get(s.BaseURL + "/sessions?period=7d&machine=machine-b")
	if err != nil {
		t.Fatalf("GET /sessions: %v", err)
	}
	sessHTML, readErr := io.ReadAll(sessResp.Body)
	_ = sessResp.Body.Close()
	if readErr != nil {
		t.Fatalf("read /sessions: %v", readErr)
	}
	if sessResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /sessions status = %d, want 200", sessResp.StatusCode)
	}
	for _, opt := range []string{"machine-b", "provider-b", "model-b", "tool-b", "project-beta"} {
		if !strings.Contains(string(sessHTML), fmt.Sprintf(`value="%s"`, opt)) {
			t.Fatalf("/sessions HTML missing immediate option %q", opt)
		}
	}
}

func TestFacets_AllFiveDimensionsSortedAndDeduplicated(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "facets-sort-tok"})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "fs-1", MachineID: "m-z", Project: "p-z", Provider: "prov-z", Model: "mod-z", Tool: "tool-z", TotalTokens: i64(10)},
		{EventID: "fs-2", MachineID: "m-a", Project: "p-a", Provider: "prov-a", Model: "mod-a", Tool: "tool-a", TotalTokens: i64(10)},
		{EventID: "fs-3", MachineID: "m-z", Project: "p-a", Provider: "prov-z", Model: "mod-a", Tool: "tool-z", TotalTokens: i64(10)},
	})
	var ana analyticsFacetsPayload
	getJSONWithClient(t, s.authClient(t), s.BaseURL+"/api/v1/analytics?period=7d&machine=m-a", &ana)

	for name, list := range map[string][]string{
		"machines":  ana.Facets.Machines,
		"providers": ana.Facets.Providers,
		"models":    ana.Facets.Models,
		"tools":     ana.Facets.Tools,
		"projects":  ana.Facets.Projects,
	} {
		if len(list) != 2 {
			t.Fatalf("facets.%s expected 2 deduplicated entries, got %v", name, list)
		}
		if !sort.StringsAreSorted(list) {
			t.Fatalf("facets.%s not sorted: %v", name, list)
		}
		for _, v := range list {
			if v == "" {
				t.Fatalf("facets.%s contains empty string", name)
			}
		}
	}
}

func TestEvolutionSWR_SchemaAndTokenCompositionAccuracy(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "evo-schema-tok"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:         "evo-1",
		MachineID:       "m-evo",
		Provider:        "openai",
		Model:           "gpt-5.4",
		Tool:            "codex",
		InputTokens:     i64(5),
		CacheReadTokens: i64(7),
		OutputTokens:    i64(3),
		TotalTokens:     i64(15),
	}})

	var ev evolutionPayload
	getJSONWithClient(t, s.authClient(t), s.BaseURL+"/api/v1/evolution", &ev)
	if ev.LifetimeTokens != 15 || ev.Stage != 1 || ev.Form != "bitling" || ev.FormName != "Bitling" || ev.LowerThreshold != 10 {
		t.Fatalf("unexpected evolution snapshot: %+v", ev)
	}
	if ev.NextThreshold == nil || *ev.NextThreshold != 100 || ev.TokensRemaining == nil || *ev.TokensRemaining != 85 {
		t.Fatalf("unexpected thresholds: next=%v rem=%v", ev.NextThreshold, ev.TokensRemaining)
	}
	if ev.Composition.InputTokens != 12 || ev.Composition.UncachedInputTokens != 5 || ev.Composition.CachedInputTokens != 7 || ev.Composition.OutputTokens != 3 {
		t.Fatalf("unexpected composition: %+v", ev.Composition)
	}
}

func TestEvolutionSWR_ServesImmediateAndConvergesAfterBurstIngest(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "evo-burst-tok"})
	auth := s.authClient(t)

	// Warm evolution cache at stage 0.
	var initEvo evolutionPayload
	getJSONWithClient(t, auth, s.BaseURL+"/api/v1/evolution", &initEvo)

	s.ingestEvents(t, []ingestEvent{
		{EventID: "eb-1", MachineID: "m1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(5), TotalTokens: i64(5)},
		{EventID: "eb-2", MachineID: "m1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(10), TotalTokens: i64(10)},
		{EventID: "eb-3", MachineID: "m1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(85), OutputTokens: i64(10), TotalTokens: i64(95)},
	})

	// Immediate poll must never fail or block.
	var imm evolutionPayload
	getJSONWithClient(t, auth, s.BaseURL+"/api/v1/evolution", &imm)

	// Poll past 2s SWR coalescing interval to confirm convergence to stage 2 (bytelet, >= 100).
	deadline := time.Now().Add(5 * time.Second)
	for {
		var cur evolutionPayload
		getJSONWithClient(t, auth, s.BaseURL+"/api/v1/evolution", &cur)
		if cur.LifetimeTokens == 110 && cur.Stage == 2 && cur.Form == "bytelet" && cur.Composition.OutputTokens == 10 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("evolution SWR did not converge within 5s; last=%+v", cur)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func TestFacets_CanonicalModelResolvesAliasesInModelFacets(t *testing.T) {
	requireE2E(t)
	catalogYAML := `schema_version: "2"
models:
  canon-gpt-e2e:
    provider: openai
    display_name: Canon GPT E2E
    tier: S
    aliases:
      - raw-gpt-alias-e2e
    pricing:
      currency: USD
      mode: standard
      verified_at: 2026-07-12
      source: https://openai.com/api/pricing/
      input_per_million: 1.0
      output_per_million: 2.0
`
	s := startTestServer(t, serverOpts{
		IngestToken: "facets-canon-tok",
		CatalogYAML: catalogYAML,
	})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "canon-1",
		MachineID:   "m-canon",
		Provider:    "openai",
		Model:       "raw-gpt-alias-e2e",
		Tool:        "codex",
		TotalTokens: i64(50),
	}})

	auth := s.authClient(t)
	var ana analyticsFacetsPayload
	getJSONWithClient(t, auth, s.BaseURL+"/api/v1/analytics?period=7d&machine=m-canon", &ana)
	if !containsAll(ana.Facets.Models, "canon-gpt-e2e") {
		t.Fatalf("facets.models missing resolved canonical model canon-gpt-e2e: %v", ana.Facets.Models)
	}
	if containsAll(ana.Facets.Models, "raw-gpt-alias-e2e") {
		t.Fatalf("facets.models leaked raw alias alongside canonical model: %v", ana.Facets.Models)
	}

	resp, err := auth.Get(s.BaseURL + "/sessions")
	if err != nil {
		t.Fatalf("GET /sessions: %v", err)
	}
	sessHTML, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read /sessions: %v", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /sessions status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(sessHTML), `value="canon-gpt-e2e"`) {
		t.Fatalf("/sessions HTML missing canonical model option canon-gpt-e2e")
	}
}

func TestFacets_EmptyFieldsDoNotPolluteFacets(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "facets-empty-tok"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:       "fac-unk-1",
		MachineID:     "m-unk",
		Provider:      "openai",
		Model:         "gpt-5.4",
		Tool:          "codex",
		TokenAccuracy: "unknown",
	}})
	var ana analyticsFacetsPayload
	getJSONWithClient(t, s.authClient(t), s.BaseURL+"/api/v1/analytics?period=7d&machine=m-unk", &ana)
	for _, p := range ana.Facets.Projects {
		if p == "" {
			t.Fatalf("facets.projects polluted with empty string: %v", ana.Facets.Projects)
		}
	}
}

func TestFacets_DuplicateOrRejectedIngestLeavesFacetsIntact(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "facets-dup-tok"})
	ev := ingestEvent{
		EventID:     "fac-dup-1",
		MachineID:   "m-orig",
		Project:     "proj-orig",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(40),
	}
	s.ingestEvents(t, []ingestEvent{ev})
	dup := s.ingestEvents(t, []ingestEvent{ev})
	if dup.Duplicates != 1 {
		t.Fatalf("expected 1 duplicate, got %+v", dup)
	}

	resp, _ := s.doReq(t, http.MethodPost, "/api/v1/events/batch", []byte(`{"events":[{"schema_version":"1","event_id":"bad-fac","timestamp":"2026-11-20T10:00:00Z","machine_id":"m-phantom","project":"/Users/bad/path","provider":"openai","model":"phantom-model","tool":"codex","total_tokens":10,"token_accuracy":"reported","source":{"adapter":"codex","adapter_version":"1"}}]}`), map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + s.IngestToken,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid batch, got %d", resp.StatusCode)
	}

	var ana analyticsFacetsPayload
	getJSONWithClient(t, s.authClient(t), s.BaseURL+"/api/v1/analytics?period=7d&machine=m-orig", &ana)
	if containsAll(ana.Facets.Machines, "m-phantom") || containsAll(ana.Facets.Models, "phantom-model") {
		t.Fatalf("rejected batch polluted facets: %+v", ana.Facets)
	}
}

func TestEvolutionSWR_ZeroStateEmptyDatabase(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "evo-zero-tok"})
	var ev evolutionPayload
	getJSONWithClient(t, s.authClient(t), s.BaseURL+"/api/v1/evolution", &ev)
	if ev.LifetimeTokens != 0 || ev.Stage != 0 || ev.Form != "egg" {
		t.Fatalf("expected stage 0 egg on empty DB, got %+v", ev)
	}
	if ev.NextThreshold == nil || *ev.NextThreshold != 10 || ev.TokensRemaining == nil || *ev.TokensRemaining != 10 {
		t.Fatalf("expected next=10 rem=10, got next=%v rem=%v", ev.NextThreshold, ev.TokensRemaining)
	}
	if ev.Composition.InputTokens != 0 || ev.Composition.OutputTokens != 0 || ev.Composition.CachedInputTokens != 0 || ev.Composition.UncachedInputTokens != 0 || ev.Composition.UnclassifiedTokens != 0 {
		t.Fatalf("expected zero composition on empty DB: %+v", ev.Composition)
	}
}

func TestEvolutionSWR_MaxStageSingularityOmitsNextThreshold(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "evo-max-tok"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "evo-singularity-1",
		MachineID:   "m-max",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(1_000_000_000_000),
	}})
	auth := s.authClient(t)
	resp, err := auth.Get(s.BaseURL + "/api/v1/evolution")
	if err != nil {
		t.Fatalf("GET /api/v1/evolution: %v", err)
	}
	raw, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read /api/v1/evolution: %v", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, string(raw))
	}
	var ev evolutionPayload
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	if ev.Stage != 18 || ev.Form != "the-singularity" || ev.Progress != 1 {
		t.Fatalf("expected stage 18 the-singularity progress 1, got %+v", ev)
	}
	if strings.Contains(string(raw), `"next_threshold"`) || strings.Contains(string(raw), `"tokens_remaining"`) {
		t.Fatalf("stage 18 JSON must omit next_threshold and tokens_remaining: %s", string(raw))
	}
}

func TestEvolutionSWR_ConcurrentPollersDuringIngestBurst(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "evo-conc-tok"})
	auth := s.authClient(t)

	var wg sync.WaitGroup
	errCh := make(chan error, 128)
	stopPoll := make(chan struct{})

	for p := 0; p < 12; p++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for {
				select {
				case <-stopPoll:
					return
				default:
				}
				resp, err := auth.Get(s.BaseURL + "/api/v1/evolution")
				if err != nil {
					errCh <- fmt.Errorf("poller %d: %w", id, err)
					return
				}
				body, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil {
					errCh <- fmt.Errorf("poller %d read: %w", id, readErr)
					return
				}
				if resp.StatusCode != http.StatusOK {
					errCh <- fmt.Errorf("poller %d status=%d", id, resp.StatusCode)
					return
				}
				var ev evolutionPayload
				if err := json.Unmarshal(body, &ev); err != nil {
					errCh <- fmt.Errorf("poller %d bad JSON: %w", id, err)
					return
				}
				time.Sleep(25 * time.Millisecond)
			}
		}(p)
	}

	for b := 0; b < 5; b++ {
		s.ingestEvents(t, []ingestEvent{{
			EventID:     fmt.Sprintf("evo-c-%d", b),
			MachineID:   "m-evo-c",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			InputTokens: i64(20),
			TotalTokens: i64(20),
		}})
		time.Sleep(40 * time.Millisecond)
	}
	close(stopPoll)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent evolution poller error: %v", err)
	}

	deadline := time.Now().Add(4 * time.Second)
	for {
		var ev evolutionPayload
		getJSONWithClient(t, auth, s.BaseURL+"/api/v1/evolution", &ev)
		if ev.LifetimeTokens == 100 && ev.Stage == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("final evolution poll after burst did not converge to 100 tokens: %+v", ev)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// --- Area F: Adapters (Codex .jsonl/.jsonl.zst, Claude Code/Desktop, Privacy) ---

type sessionRow struct {
	SessionID    string `json:"session_id"`
	Machine      string `json:"machine"`
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Tool         string `json:"tool"`
	Project      string `json:"project"`
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
	TotalTokens  *int64 `json:"total_tokens"`
	DurationMS   *int64 `json:"duration_ms"`
	Accuracy     string `json:"accuracy"`
	Events       int64  `json:"events"`
}

func runAgentWithArgs(t *testing.T, s *testServer, agentHome, stateDB, machineID, adapters string, extraArgs ...string) (string, string, error) {
	t.Helper()
	emptyCfg := filepath.Join(t.TempDir(), "no-agent.env")
	args := []string{
		"agent", "--once",
		"--config", emptyCfg,
		"--server", s.BaseURL,
		"--token", s.IngestToken,
		"--machine-id", machineID,
		"--home", agentHome,
		"--state", stateDB,
		"--adapters", adapters,
	}
	args = append(args, extraArgs...)
	return s.runCLI(t, nil, args...)
}

func TestE2ECodexActiveJSONLIngestion(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "cdx-active-tok"})
	agentHome := filepath.Join(t.TempDir(), "agent-home")
	stateDB := filepath.Join(t.TempDir(), "state.db")
	base := time.Now().UTC().Add(-2 * time.Hour)

	rollout := filepath.Join(agentHome, ".codex", "sessions", "2026", "11", "20", "rollout-active.jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o755); err != nil {
		t.Fatalf("mkdir rollout: %v", err)
	}
	content := strings.Join([]string{
		fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":"codex-sess-001","model_provider":"openai","cwd":"/Users/alice/secret-repo"}}`, base.Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"model":"gpt-5.4","cwd":"/Users/alice/secret-repo"}}`, base.Add(time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":200,"reasoning_output_tokens":50,"total_tokens":1200}}}}`, base.Add(2*time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1500,"cached_input_tokens":900,"output_tokens":300,"reasoning_output_tokens":75,"total_tokens":1800}}}}`, base.Add(3*time.Second).Format(time.RFC3339)),
	}, "\n") + "\n"
	if err := os.WriteFile(rollout, []byte(content), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	_, stderr, err := runAgentWithArgs(t, s, agentHome, stateDB, "m-cdx-active", "codex")
	if err != nil {
		t.Fatalf("agent --once failed: %v\nstderr: %s", err, stderr)
	}

	auth := s.authClient(t)
	var sessions []sessionRow
	getJSONWithClient(t, auth, s.BaseURL+"/api/v1/sessions?tool=codex", &sessions)
	if len(sessions) != 1 {
		t.Fatalf("expected 1 codex session, got %d: %+v", len(sessions), sessions)
	}
	got := sessions[0]
	if got.SessionID != "codex-sess-001" || got.Project != "secret-repo" || got.Accuracy != "reported" || got.Events != 2 {
		t.Fatalf("unexpected codex session metadata: %+v", got)
	}
	if got.TotalTokens == nil || *got.TotalTokens != 1800 || got.InputTokens == nil || *got.InputTokens != 600 || got.OutputTokens == nil || *got.OutputTokens != 300 {
		t.Fatalf("unexpected codex session tokens: %+v", got)
	}

	waitOverviewLifetimeTokens(t, auth, s.BaseURL, 1800)
}

func TestE2ECodexArchivedZstIngestion(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "cdx-zst-tok"})
	agentHome := filepath.Join(t.TempDir(), "agent-home")
	stateDB := filepath.Join(t.TempDir(), "state.db")
	base := time.Now().UTC().Add(-2 * time.Hour)

	zstPath := filepath.Join(agentHome, ".codex", "archived_sessions", "rollout-arch.jsonl.zst")
	payload := strings.Join([]string{
		fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":"codex-arch-001","model_provider":"openai","cwd":"/Users/alice/secret-repo"}}`, base.Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"model":"gpt-5.4","cwd":"/Users/alice/secret-repo"}}`, base.Add(time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":2000,"cached_input_tokens":1000,"output_tokens":500,"reasoning_output_tokens":0,"total_tokens":2500},"total_token_usage":{"input_tokens":2000,"cached_input_tokens":1000,"output_tokens":500,"reasoning_output_tokens":0,"total_tokens":2500}}}}`, base.Add(2*time.Second).Format(time.RFC3339)),
	}, "\n") + "\n"
	writeRawZstdFrame(t, zstPath, []byte(payload))

	_, stderr, err := runAgentWithArgs(t, s, agentHome, stateDB, "m-cdx-arch", "codex")
	if err != nil {
		t.Fatalf("agent --once failed: %v\nstderr: %s", err, stderr)
	}

	auth := s.authClient(t)
	var sessions []sessionRow
	getJSONWithClient(t, auth, s.BaseURL+"/api/v1/sessions?tool=codex", &sessions)
	if len(sessions) != 1 || sessions[0].TotalTokens == nil || *sessions[0].TotalTokens != 2500 || sessions[0].Project != "secret-repo" {
		t.Fatalf("unexpected archived zst session: %+v", sessions)
	}

	var ana struct {
		Summary struct {
			Tokens int64 `json:"tokens"`
		} `json:"summary"`
		Facets struct {
			Tools []string `json:"tools"`
		} `json:"facets"`
	}
	getJSONWithClient(t, auth, s.BaseURL+"/api/v1/analytics?dimension=harnesses", &ana)
	if ana.Summary.Tokens != 2500 || !containsAll(ana.Facets.Tools, "codex") {
		t.Fatalf("unexpected analytics for archived zst: %+v", ana)
	}
}

func TestE2ECodexActiveToArchivedZstDeduplication(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "cdx-dedup-zst-tok"})
	agentHome := filepath.Join(t.TempDir(), "agent-home")
	stateDB := filepath.Join(t.TempDir(), "state.db")
	base := time.Now().UTC().Add(-2 * time.Hour)

	rawJSONL := []byte(strings.Join([]string{
		fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":"codex-move-001","model_provider":"openai","cwd":"/Users/alice/secret-repo"}}`, base.Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"model":"gpt-5.4","cwd":"/Users/alice/secret-repo"}}`, base.Add(time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":200,"reasoning_output_tokens":0,"total_tokens":1200},"total_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":200,"reasoning_output_tokens":0,"total_tokens":1200}}}}`, base.Add(2*time.Second).Format(time.RFC3339)),
	}, "\n") + "\n")

	activePath := filepath.Join(agentHome, ".codex", "sessions", "2026", "11", "20", "rollout-move.jsonl")
	if err := os.MkdirAll(filepath.Dir(activePath), 0o755); err != nil {
		t.Fatalf("mkdir active codex dir: %v", err)
	}
	if err := os.WriteFile(activePath, rawJSONL, 0o644); err != nil {
		t.Fatalf("write active rollout: %v", err)
	}

	if _, stderr, err := runAgentWithArgs(t, s, agentHome, stateDB, "m-move", "codex"); err != nil {
		t.Fatalf("run 1 failed: %v\nstderr: %s", err, stderr)
	}

	archPath := filepath.Join(agentHome, ".codex", "archived_sessions", "rollout-move.jsonl.zst")
	writeRawZstdFrame(t, archPath, rawJSONL)
	if err := os.Remove(activePath); err != nil {
		t.Fatalf("remove active rollout: %v", err)
	}
	if _, stderr, err := runAgentWithArgs(t, s, agentHome, stateDB, "m-move", "codex"); err != nil {
		t.Fatalf("run 2 failed: %v\nstderr: %s", err, stderr)
	}

	// Coexisting active + archived with identical content.
	if err := os.WriteFile(activePath, rawJSONL, 0o644); err != nil {
		t.Fatalf("rewrite active rollout: %v", err)
	}
	if _, stderr, err := runAgentWithArgs(t, s, agentHome, stateDB, "m-move", "codex"); err != nil {
		t.Fatalf("run 3 failed: %v\nstderr: %s", err, stderr)
	}

	auth := s.authClient(t)
	waitOverviewLifetimeTokens(t, auth, s.BaseURL, 1200)
	var sessions []sessionRow
	getJSONWithClient(t, auth, s.BaseURL+"/api/v1/sessions?tool=codex", &sessions)
	if len(sessions) != 1 || sessions[0].Events != 1 || sessions[0].TotalTokens == nil || *sessions[0].TotalTokens != 1200 {
		t.Fatalf("expected 1 deduped session with 1 event / 1200 tokens, got %+v", sessions)
	}
}

func TestE2EClaudeCodeAndDesktopHarnessSeparation(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "claude-sep-tok"})
	agentHome := filepath.Join(t.TempDir(), "agent-home")
	stateDB := filepath.Join(t.TempDir(), "state.db")
	base := time.Now().UTC().Add(-2 * time.Hour)

	projDir := filepath.Join(agentHome, ".claude", "projects", "-Users-alice-secret-repo")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir claude projDir: %v", err)
	}

	cliJSONL := strings.Join([]string{
		fmt.Sprintf(`{"type":"user","timestamp":%q,"sessionId":"claude-cli-001","cwd":"/Users/alice/secret-repo","slug":"SECRET_TITLE_CLI","message":{"role":"user","content":"SECRET_PROMPT_CLI"}}`, base.Format(time.RFC3339)),
		fmt.Sprintf(`{"type":"assistant","timestamp":%q,"sessionId":"claude-cli-001","cwd":"/Users/alice/secret-repo","slug":"SECRET_TITLE_CLI","message":{"model":"claude-sonnet-4-5","content":"SECRET_RESPONSE_CLI","usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":300,"cache_creation_input_tokens":40}}}`, base.Add(time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"type":"system","subtype":"turn_duration","timestamp":%q,"sessionId":"claude-cli-001","durationMs":1200}`, base.Add(2*time.Second).Format(time.RFC3339)),
	}, "\n") + "\n"
	deskJSONL := strings.Join([]string{
		fmt.Sprintf(`{"type":"user","timestamp":%q,"sessionId":"claude-desk-001","cwd":"/Users/alice/secret-repo","slug":"SECRET_TITLE_DESKTOP","entrypoint":"claude-desktop","message":{"role":"user","content":"SECRET_PROMPT_DESKTOP"}}`, base.Add(5*time.Minute).Format(time.RFC3339)),
		fmt.Sprintf(`{"type":"assistant","timestamp":%q,"sessionId":"claude-desk-001","cwd":"/Users/alice/secret-repo","slug":"SECRET_TITLE_DESKTOP","entrypoint":"claude-desktop","message":{"model":"claude-sonnet-4-5","content":"SECRET_RESPONSE_DESKTOP","usage":{"input_tokens":200,"output_tokens":80,"cache_read_input_tokens":500,"cache_creation_input_tokens":20}}}`, base.Add(5*time.Minute+time.Second).Format(time.RFC3339)),
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(projDir, "cli.jsonl"), []byte(cliJSONL), 0o644); err != nil {
		t.Fatalf("write cli.jsonl: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "desktop.jsonl"), []byte(deskJSONL), 0o644); err != nil {
		t.Fatalf("write desktop.jsonl: %v", err)
	}

	_, stderr, err := runAgentWithArgs(t, s, agentHome, stateDB, "m-claude-sep", "claude-code")
	if err != nil {
		t.Fatalf("agent --once claude-code failed: %v\nstderr: %s", err, stderr)
	}

	auth := s.authClient(t)
	var allSessions []sessionRow
	getJSONWithClient(t, auth, s.BaseURL+"/api/v1/sessions", &allSessions)
	if len(allSessions) != 2 {
		t.Fatalf("expected 2 claude sessions, got %+v", allSessions)
	}

	var deskOnly []sessionRow
	getJSONWithClient(t, auth, s.BaseURL+"/api/v1/sessions?tool=claude-desktop", &deskOnly)
	if len(deskOnly) != 1 || deskOnly[0].SessionID != "claude-desk-001" || deskOnly[0].TotalTokens == nil || *deskOnly[0].TotalTokens != 800 {
		t.Fatalf("expected only claude-desk-001 (800 tokens), got %+v", deskOnly)
	}

	waitOverviewLifetimeTokens(t, auth, s.BaseURL, 1290)
	var ov struct {
		ByTool []struct {
			Tool   string `json:"tool"`
			Tokens int64  `json:"tokens"`
		} `json:"by_tool"`
	}
	getJSONWithClient(t, auth, s.BaseURL+"/api/v1/analytics/overview", &ov)
	toolTotals := map[string]int64{}
	for _, bt := range ov.ByTool {
		toolTotals[bt.Tool] = bt.Tokens
	}
	if toolTotals["claude-code"] != 490 || toolTotals["claude-desktop"] != 800 {
		t.Fatalf("unexpected overview by_tool: %+v", ov.ByTool)
	}

	var ana struct {
		Facets struct {
			Tools []string `json:"tools"`
		} `json:"facets"`
		Breakdown []struct {
			Name   string `json:"name"`
			Tokens int64  `json:"tokens"`
		} `json:"breakdown"`
	}
	getJSONWithClient(t, auth, s.BaseURL+"/api/v1/analytics?dimension=harnesses", &ana)
	if !containsAll(ana.Facets.Tools, "claude-code", "claude-desktop") {
		t.Fatalf("facets.tools missing claude-code/claude-desktop: %v", ana.Facets.Tools)
	}

	for _, page := range []string{"/analytics?dimension=harnesses", "/sessions"} {
		resp, err := auth.Get(s.BaseURL + page)
		if err != nil {
			t.Fatalf("GET %s: %v", page, err)
		}
		htmlBytes, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status=%d err=%v", page, resp.StatusCode, readErr)
		}
		html := string(htmlBytes)
		if !strings.Contains(html, `value="claude-desktop"`) || !strings.Contains(html, `value="claude-code"`) {
			t.Fatalf("%s HTML missing claude-desktop / claude-code <option>", page)
		}
	}
}

func TestE2EAdapterIncrementalFacetsImmediate(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "adap-fac-tok"})
	auth := s.authClient(t)

	// Warm /api/v1/analytics before ingest (facets empty).
	var pre analyticsFacetsPayload
	getJSONWithClient(t, auth, s.BaseURL+"/api/v1/analytics", &pre)
	if len(pre.Facets.Tools) != 0 {
		t.Fatalf("expected empty pre-ingest facets.tools, got %v", pre.Facets.Tools)
	}

	agentHome := filepath.Join(t.TempDir(), "agent-home")
	stateDB := filepath.Join(t.TempDir(), "state.db")
	base := time.Now().UTC().Add(-2 * time.Hour)

	zstPayload := strings.Join([]string{
		fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":"af-cdx-1","model_provider":"openai","cwd":"/Users/alice/secret-repo"}}`, base.Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"model":"gpt-5.4","cwd":"/Users/alice/secret-repo"}}`, base.Add(time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":500,"cached_input_tokens":200,"output_tokens":100,"reasoning_output_tokens":0,"total_tokens":600}}}}`, base.Add(2*time.Second).Format(time.RFC3339)),
	}, "\n") + "\n"
	writeRawZstdFrame(t, filepath.Join(agentHome, ".codex", "archived_sessions", "r1.jsonl.zst"), []byte(zstPayload))

	claudeFile := filepath.Join(agentHome, ".claude", "projects", "-Users-alice-secret-repo", "desk.jsonl")
	if err := os.MkdirAll(filepath.Dir(claudeFile), 0o755); err != nil {
		t.Fatalf("mkdir claude dir: %v", err)
	}
	claudeLine := fmt.Sprintf(`{"type":"assistant","timestamp":%q,"sessionId":"af-desk-1","cwd":"/Users/alice/secret-repo","entrypoint":"claude-desktop","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`+"\n", base.Add(5*time.Minute).Format(time.RFC3339))
	if err := os.WriteFile(claudeFile, []byte(claudeLine), 0o644); err != nil {
		t.Fatalf("write claude desk.jsonl: %v", err)
	}

	_, stderr, err := runAgentWithArgs(t, s, agentHome, stateDB, "m-adap-fac", "codex,claude-code")
	if err != nil {
		t.Fatalf("agent --once failed: %v\nstderr: %s", err, stderr)
	}

	var post analyticsFacetsPayload
	getJSONWithClient(t, auth, s.BaseURL+"/api/v1/analytics?period=7d&machine=m-adap-fac", &post)
	if !containsAll(post.Facets.Tools, "codex", "claude-desktop") || !containsAll(post.Facets.Machines, "m-adap-fac") {
		t.Fatalf("incremental facets did not reflect adapter ingest immediately: %+v", post.Facets)
	}
	if len(post.Facets.Models) < 2 {
		t.Fatalf("expected >= 2 resolved models in facets.models, got %v", post.Facets.Models)
	}
}

func TestE2EAdapterPrivacyStrippingAcrossEndpoints(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "adap-priv-tok"})
	agentHome := filepath.Join(t.TempDir(), "agent-home")
	stateDB := filepath.Join(t.TempDir(), "state.db")
	base := time.Now().UTC().Add(-2 * time.Hour)

	zstPayload := strings.Join([]string{
		fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":"priv-cdx-1","model_provider":"openai","cwd":"/Users/alice/secret-repo","base_instructions":"SECRET_SYSTEM_INSTRUCTIONS"}}`, base.Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"model":"gpt-5.4","cwd":"/Users/alice/secret-repo","summary":"SECRET_TITLE_DO_NOT_LEAK"}}`, base.Add(time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"SECRET_PROMPT_DO_NOT_LEAK"}]}}`, base.Add(2*time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":500,"cached_input_tokens":200,"output_tokens":100,"reasoning_output_tokens":0,"total_tokens":600}}}}`, base.Add(3*time.Second).Format(time.RFC3339)),
	}, "\n") + "\n"
	writeRawZstdFrame(t, filepath.Join(agentHome, ".codex", "archived_sessions", "secret-meeting-notes-2026.jsonl.zst"), []byte(zstPayload))

	claudeFile := filepath.Join(agentHome, ".claude", "projects", "-Users-alice-secret-repo", "secret-meeting-notes-2026.jsonl")
	if err := os.MkdirAll(filepath.Dir(claudeFile), 0o755); err != nil {
		t.Fatalf("mkdir claude dir: %v", err)
	}
	claudeContent := strings.Join([]string{
		fmt.Sprintf(`{"type":"user","timestamp":%q,"cwd":"/Users/alice/secret-repo","slug":"SECRET_TITLE_DO_NOT_LEAK","entrypoint":"claude-desktop","message":{"role":"user","content":"SECRET_PROMPT_DO_NOT_LEAK"}}`, base.Add(5*time.Minute).Format(time.RFC3339)),
		fmt.Sprintf(`{"type":"assistant","timestamp":%q,"cwd":"/Users/alice/secret-repo","slug":"SECRET_TITLE_DO_NOT_LEAK","entrypoint":"claude-desktop","message":{"model":"claude-sonnet-4-5","content":"SECRET_RESPONSE_DO_NOT_LEAK","usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`, base.Add(5*time.Minute+time.Second).Format(time.RFC3339)),
	}, "\n") + "\n"
	if err := os.WriteFile(claudeFile, []byte(claudeContent), 0o644); err != nil {
		t.Fatalf("write claude file: %v", err)
	}

	_, stderr, err := runAgentWithArgs(t, s, agentHome, stateDB, "m-priv-e2e", "codex,claude-code")
	if err != nil {
		t.Fatalf("agent --once failed: %v\nstderr: %s", err, stderr)
	}

	auth := s.authClient(t)
	forbidden := []string{
		"/Users/alice",
		agentHome,
		"SECRET_PROMPT_DO_NOT_LEAK",
		"SECRET_RESPONSE_DO_NOT_LEAK",
		"SECRET_TITLE_DO_NOT_LEAK",
		"SECRET_SYSTEM_INSTRUCTIONS",
		"secret-meeting-notes-2026",
	}
	for _, path := range []string{
		"/api/v1/sessions",
		"/api/v1/analytics/overview",
		"/api/v1/analytics",
		"/api/v1/machines",
		"/api/v1/evolution",
		"/",
		"/analytics",
		"/sessions",
	} {
		resp, err := auth.Get(s.BaseURL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d", path, resp.StatusCode)
		}
		for _, f := range forbidden {
			if strings.Contains(string(body), f) {
				t.Fatalf("endpoint %s leaked %q", path, f)
			}
		}
	}

	var sessions []sessionRow
	getJSONWithClient(t, auth, s.BaseURL+"/api/v1/sessions", &sessions)
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
	for _, sess := range sessions {
		if sess.Project != "secret-repo" {
			t.Fatalf("expected normalized project secret-repo, got %q", sess.Project)
		}
		if sess.Tool == "claude-desktop" && !strings.HasPrefix(sess.SessionID, "sha256:") {
			t.Fatalf("expected hashed fallback session_id for ID-less claude file, got %q", sess.SessionID)
		}
	}
}

func TestE2ECorruptZstIsolatesErrorAndPreservesHealthy(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "zst-err-iso-tok"})
	agentHome := filepath.Join(t.TempDir(), "agent-home")
	stateDB := filepath.Join(t.TempDir(), "state.db")

	writeAgentCodexRollout(t, agentHome, filepath.Join("sessions", "2026", "11", "20", "healthy.jsonl"), "cdx-healthy-1", 1200)
	corruptPath := filepath.Join(agentHome, ".codex", "archived_sessions", "corrupt.jsonl.zst")
	if err := os.MkdirAll(filepath.Dir(corruptPath), 0o755); err != nil {
		t.Fatalf("mkdir corrupt dir: %v", err)
	}
	if err := os.WriteFile(corruptPath, []byte("not-zstd-data"), 0o644); err != nil {
		t.Fatalf("write corrupt zst: %v", err)
	}

	_, _, err := runAgentWithArgs(t, s, agentHome, stateDB, "m-corrupt-zst", "codex")
	if err == nil {
		t.Fatalf("expected agent --once to return non-zero exit when corrupt .jsonl.zst is present")
	}

	auth := s.authClient(t)
	waitOverviewLifetimeTokens(t, auth, s.BaseURL, 1200)

	var machines []struct {
		SourceCount      int `json:"source_count"`
		SourceErrorCount int `json:"source_error_count"`
		Sources          []struct {
			State          string `json:"state"`
			LastErrorClass string `json:"last_error_class"`
		} `json:"sources"`
	}
	getJSONWithClient(t, auth, s.BaseURL+"/api/v1/machines", &machines)
	if len(machines) != 1 || machines[0].SourceCount != 2 || machines[0].SourceErrorCount != 1 {
		t.Fatalf("unexpected machine source error counts: %+v", machines)
	}
	var hasFresh, hasErr bool
	for _, src := range machines[0].Sources {
		if src.State == "Fresh" {
			hasFresh = true
		}
		if src.LastErrorClass == "other" {
			hasErr = true
		}
	}
	if !hasFresh || !hasErr {
		t.Fatalf("expected both Fresh and error_class=other sources, got %+v", machines[0].Sources)
	}
}

func TestE2EEmptyZstArchiveHandledGracefully(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "zst-empty-tok"})
	agentHome := filepath.Join(t.TempDir(), "agent-home")
	stateDB := filepath.Join(t.TempDir(), "state.db")

	writeRawZstdFrame(t, filepath.Join(agentHome, ".codex", "archived_sessions", "empty.jsonl.zst"), nil)
	emptyJSONL := filepath.Join(agentHome, ".codex", "sessions", "empty.jsonl")
	if err := os.MkdirAll(filepath.Dir(emptyJSONL), 0o755); err != nil {
		t.Fatalf("mkdir empty jsonl dir: %v", err)
	}
	if err := os.WriteFile(emptyJSONL, nil, 0o644); err != nil {
		t.Fatalf("write empty jsonl: %v", err)
	}

	_, stderr, err := runAgentWithArgs(t, s, agentHome, stateDB, "m-empty-zst", "codex")
	if err != nil {
		t.Fatalf("empty zst archive caused failure: %v\nstderr: %s", err, stderr)
	}

	auth := s.authClient(t)
	var ov struct {
		LifetimeTokens int64 `json:"lifetime_tokens"`
	}
	getJSONWithClient(t, auth, s.BaseURL+"/api/v1/analytics/overview", &ov)
	if ov.LifetimeTokens != 0 {
		t.Fatalf("expected 0 tokens for empty archives, got %d", ov.LifetimeTokens)
	}
	var machines []struct {
		SourceErrorCount int `json:"source_error_count"`
	}
	getJSONWithClient(t, auth, s.BaseURL+"/api/v1/machines", &machines)
	if len(machines) != 1 || machines[0].SourceErrorCount != 0 {
		t.Fatalf("expected 0 source errors on empty archives, got %+v", machines)
	}
}

func TestE2EMalformedJSONLLineSkippedWithoutDataLoss(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "malformed-jsonl-tok"})
	agentHome := filepath.Join(t.TempDir(), "agent-home")
	stateDB := filepath.Join(t.TempDir(), "state.db")
	base := time.Now().UTC().Add(-2 * time.Hour)

	zstContent := strings.Join([]string{
		fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":"mal-cdx-1","model_provider":"openai","cwd":"/Users/alice/secret-repo"}}`, base.Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"model":"gpt-5.4","cwd":"/Users/alice/secret-repo"}}`, base.Add(time.Second).Format(time.RFC3339)),
		`{broken json`,
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":200,"reasoning_output_tokens":0,"total_tokens":1200}}}}`, base.Add(2*time.Second).Format(time.RFC3339)),
	}, "\n") + "\n"
	writeRawZstdFrame(t, filepath.Join(agentHome, ".codex", "archived_sessions", "mal.jsonl.zst"), []byte(zstContent))

	claudePath := filepath.Join(agentHome, ".claude", "projects", "-Users-alice-secret-repo", "partial.jsonl")
	if err := os.MkdirAll(filepath.Dir(claudePath), 0o755); err != nil {
		t.Fatalf("mkdir claude dir: %v", err)
	}
	incompleteTail := fmt.Sprintf(`{"type":"assistant","timestamp":%q,"sessionId":"mal-claude-1","cwd":"/Users/alice/secret-repo","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":200,`, base.Add(5*time.Minute+time.Second).Format(time.RFC3339))
	initialClaude := strings.Join([]string{
		fmt.Sprintf(`{"type":"assistant","timestamp":%q,"sessionId":"mal-claude-1","cwd":"/Users/alice/secret-repo","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":300,"cache_creation_input_tokens":40}}}`, base.Add(5*time.Minute).Format(time.RFC3339)),
		`{broken json`,
	}, "\n") + "\n" + incompleteTail
	if err := os.WriteFile(claudePath, []byte(initialClaude), 0o644); err != nil {
		t.Fatalf("write partial claude jsonl: %v", err)
	}

	_, stderr, err := runAgentWithArgs(t, s, agentHome, stateDB, "m-malformed", "codex,claude-code")
	if err != nil {
		t.Fatalf("pass 1 with malformed/incomplete lines failed: %v\nstderr: %s", err, stderr)
	}

	auth := s.authClient(t)
	waitOverviewLifetimeTokens(t, auth, s.BaseURL, 1690)

	// Complete the trailing line (including cache_read_input_tokens & cache_creation_input_tokens per claude.go) and re-run.
	f, err := os.OpenFile(claudePath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open claude file for append: %v", err)
	}
	if _, err := f.WriteString(`"output_tokens":100,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}` + "\n"); err != nil {
		_ = f.Close()
		t.Fatalf("append completed claude line: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close claude file: %v", err)
	}

	_, stderr2, err2 := runAgentWithArgs(t, s, agentHome, stateDB, "m-malformed", "codex,claude-code")
	if err2 != nil {
		t.Fatalf("pass 2 after completing trailing line failed: %v\nstderr: %s", err2, stderr2)
	}
	waitOverviewLifetimeTokens(t, auth, s.BaseURL, 1990)
}

func TestE2EClaudeEntrypointDesktopAndNonDesktop(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "claude-ep-tok"})
	agentHome := filepath.Join(t.TempDir(), "agent-home")
	stateDB := filepath.Join(t.TempDir(), "state.db")
	base := time.Now().UTC().Add(-2 * time.Hour)

	projDir := filepath.Join(agentHome, ".claude", "projects", "-Users-alice-secret-repo")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir claude dir: %v", err)
	}
	casesJSONL := strings.Join([]string{
		fmt.Sprintf(`{"type":"assistant","timestamp":%q,"sessionId":"case-desktop","cwd":"/Users/alice/secret-repo","entrypoint":"claude-desktop","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`, base.Format(time.RFC3339)),
		fmt.Sprintf(`{"type":"assistant","timestamp":%q,"sessionId":"case-cli","cwd":"/Users/alice/secret-repo","entrypoint":"cli","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":20,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`, base.Add(time.Minute).Format(time.RFC3339)),
		fmt.Sprintf(`{"type":"assistant","timestamp":%q,"sessionId":"case-empty","cwd":"/Users/alice/secret-repo","entrypoint":"","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":30,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`, base.Add(2*time.Minute).Format(time.RFC3339)),
		fmt.Sprintf(`{"type":"assistant","timestamp":%q,"sessionId":"case-absent","cwd":"/Users/alice/secret-repo","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":40,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`, base.Add(3*time.Minute).Format(time.RFC3339)),
		fmt.Sprintf(`{"type":"assistant","timestamp":%q,"sessionId":"case-desktop-other","cwd":"/Users/alice/secret-repo","entrypoint":"claude-desktop-other","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":50,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`, base.Add(4*time.Minute).Format(time.RFC3339)),
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(projDir, "cases.jsonl"), []byte(casesJSONL), 0o644); err != nil {
		t.Fatalf("write cases.jsonl: %v", err)
	}

	_, stderr, err := runAgentWithArgs(t, s, agentHome, stateDB, "m-case", "claude-code")
	if err != nil {
		t.Fatalf("agent --once failed: %v\nstderr: %s", err, stderr)
	}

	var sessions []sessionRow
	getJSONWithClient(t, s.authClient(t), s.BaseURL+"/api/v1/sessions", &sessions)
	byID := map[string]string{}
	for _, row := range sessions {
		byID[row.SessionID] = row.Tool
	}
	if byID["case-desktop"] != "claude-desktop" {
		t.Fatalf("entrypoint=claude-desktop mapped to %q, want claude-desktop", byID["case-desktop"])
	}
	for _, id := range []string{"case-cli", "case-empty", "case-absent", "case-desktop-other"} {
		if byID[id] != "claude-code" {
			t.Fatalf("%s mapped to %q, want claude-code", id, byID[id])
		}
	}
}

func TestE2EAgentIncludeProjectsFalseStripsBasename(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "noproj-tok"})
	agentHome := filepath.Join(t.TempDir(), "agent-home")
	stateDB := filepath.Join(t.TempDir(), "state.db")
	base := time.Now().UTC().Add(-2 * time.Hour)

	zstPayload := strings.Join([]string{
		fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":"np-cdx-1","model_provider":"openai","cwd":"/Users/alice/top-secret-codename"}}`, base.Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"model":"gpt-5.4","cwd":"/Users/alice/top-secret-codename"}}`, base.Add(time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":50,"reasoning_output_tokens":0,"total_tokens":150}}}}`, base.Add(2*time.Second).Format(time.RFC3339)),
	}, "\n") + "\n"
	writeRawZstdFrame(t, filepath.Join(agentHome, ".codex", "archived_sessions", "np.jsonl.zst"), []byte(zstPayload))

	claudeFile := filepath.Join(agentHome, ".claude", "projects", "-Users-alice-top-secret-codename", "np.jsonl")
	if err := os.MkdirAll(filepath.Dir(claudeFile), 0o755); err != nil {
		t.Fatalf("mkdir claude dir: %v", err)
	}
	claudeNP := fmt.Sprintf(`{"type":"assistant","timestamp":%q,"sessionId":"np-claude-1","cwd":"/Users/alice/top-secret-codename","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`+"\n", base.Add(5*time.Minute).Format(time.RFC3339))
	if err := os.WriteFile(claudeFile, []byte(claudeNP), 0o644); err != nil {
		t.Fatalf("write claude np.jsonl: %v", err)
	}

	_, stderr, err := runAgentWithArgs(t, s, agentHome, stateDB, "m-noproj", "codex,claude-code", "--include-projects=false")
	if err != nil {
		t.Fatalf("agent --once --include-projects=false failed: %v\nstderr: %s", err, stderr)
	}

	auth := s.authClient(t)
	var sessions []sessionRow
	getJSONWithClient(t, auth, s.BaseURL+"/api/v1/sessions", &sessions)
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
	for _, row := range sessions {
		if row.Project != "" {
			t.Fatalf("expected empty project with --include-projects=false, got %q", row.Project)
		}
	}

	for _, path := range []string{"/api/v1/sessions", "/api/v1/analytics", "/analytics"} {
		resp, err := auth.Get(s.BaseURL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status=%d err=%v", path, resp.StatusCode, readErr)
		}
		if strings.Contains(string(body), "top-secret-codename") {
			t.Fatalf("%s leaked project basename top-secret-codename when --include-projects=false", path)
		}
	}
}

func TestE2ECodexMultiBlockLargeZstFrame(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "zst-multiblock-tok"})
	agentHome := filepath.Join(t.TempDir(), "agent-home")
	stateDB := filepath.Join(t.TempDir(), "state.db")
	base := time.Now().UTC().Add(-2 * time.Hour)

	var buf bytes.Buffer
	fmt.Fprintf(&buf, `{"timestamp":%q,"type":"session_meta","payload":{"id":"cdx-big-001","model_provider":"openai","cwd":"/Users/alice/secret-repo"}}`+"\n", base.Format(time.RFC3339))
	fmt.Fprintf(&buf, `{"timestamp":%q,"type":"turn_context","payload":{"model":"gpt-5.4","cwd":"/Users/alice/secret-repo"}}`+"\n", base.Add(time.Second).Format(time.RFC3339))
	pad := strings.Repeat("A", 800)
	for i := 0; i < 220; i++ {
		fmt.Fprintf(&buf, `{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}}`+"\n", base.Add(2*time.Second).Format(time.RFC3339), pad)
	}
	fmt.Fprintf(&buf, `{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":3000,"cached_input_tokens":1000,"output_tokens":500,"reasoning_output_tokens":0,"total_tokens":3500}}}}`+"\n", base.Add(3*time.Second).Format(time.RFC3339))

	if buf.Len() <= 128*1024 {
		t.Fatalf("expected multi-block payload >128KB, got %d bytes", buf.Len())
	}
	writeRawZstdFrame(t, filepath.Join(agentHome, ".codex", "archived_sessions", "large.jsonl.zst"), buf.Bytes())

	_, stderr, err := runAgentWithArgs(t, s, agentHome, stateDB, "m-multiblock", "codex")
	if err != nil {
		t.Fatalf("agent --once multi-block zst failed: %v\nstderr: %s", err, stderr)
	}

	waitOverviewLifetimeTokens(t, s.authClient(t), s.BaseURL, 3500)
}
