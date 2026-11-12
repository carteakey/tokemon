package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"golang.org/x/crypto/bcrypt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/usage"
)

func securityEvent(id string) usage.Event {
	return usage.Event{
		SchemaVersion: usage.SchemaVersion,
		EventID:       id,
		Timestamp:     time.Now().UTC(),
		MachineID:     "machine",
		Provider:      "provider",
		Model:         "model",
		Tool:          "tool",
		TotalTokens:   usage.Int64(10),
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "test", AdapterVersion: "1"},
	}
}

func postBatch(t *testing.T, server *Server, events []usage.Event, token, remote string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(batchRequest{Events: events})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/events/batch", bytes.NewReader(payload))
	request.RemoteAddr = remote
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func TestEmptyIngestTokenFailsClosedOutsideLoopback(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	strict, err := NewWithConfig(store, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if response := postBatch(t, strict, []usage.Event{securityEvent("strict")}, "", "192.0.2.1:1"); response.Code != http.StatusUnauthorized {
		t.Fatalf("strict empty-token status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	dev, err := NewWithConfig(store, Config{AllowLoopbackDev: true})
	if err != nil {
		t.Fatal(err)
	}
	if response := postBatch(t, dev, []usage.Event{securityEvent("remote")}, "", "192.0.2.1:1"); response.Code != http.StatusUnauthorized {
		t.Fatalf("remote dev empty-token status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if response := postBatch(t, dev, []usage.Event{securityEvent("loopback")}, "", "127.0.0.1:1"); response.Code != http.StatusOK {
		t.Fatalf("loopback dev empty-token status = %d: %s", response.Code, response.Body.String())
	}
	withToken, err := NewWithConfig(store, Config{IngestToken: "secret", AllowLoopbackDev: true})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "127.0.0.1:1"
	response := httptest.NewRecorder()
	withToken.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("loopback with configured token dashboard status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestServeConfigRejectsUnsafeEmptyToken(t *testing.T) {
	if err := ValidateServeConfig(":8080", "", true); err == nil {
		t.Fatal("wildcard dev listener should be rejected")
	}
	if err := ValidateServeConfig("127.0.0.1:8080", "", true); err != nil {
		t.Fatalf("loopback dev listener rejected: %v", err)
	}
	if err := ValidateServeConfig("127.0.0.1:8080", "", false); err == nil {
		t.Fatal("empty token without explicit dev mode should be rejected")
	}
}

func TestInvalidBatchDoesNotMutateDatabase(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := NewWithConfig(store, Config{IngestToken: "secret", DashboardToken: "dashboard"})
	if err != nil {
		t.Fatal(err)
	}
	if response := postBatch(t, server, []usage.Event{securityEvent("valid"), func() usage.Event {
		invalid := securityEvent("invalid")
		invalid.TotalTokens = usage.Int64(1)
		invalid.InputTokens = usage.Int64(2)
		return invalid
	}()}, "secret", "192.0.2.1:1"); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid batch status = %d, want %d: %s", response.Code, http.StatusBadRequest, response.Body.String())
	}
	events, err := store.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("rejected batch mutated database: %+v", events)
	}
}

func TestGzipExpansionAndEventCountLimits(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := NewWithConfig(store, Config{IngestToken: "secret", DashboardToken: "dashboard"})
	if err != nil {
		t.Fatal(err)
	}
	large := bytes.Repeat([]byte("x"), int(maxDecompressedBodyBytes))
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	payload := append([]byte(`{"events":[{"metadata":{"x":"`), large...)
	payload = append(payload, []byte(`"}}]}`)...)
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/events/batch", &compressed)
	request.RemoteAddr = "192.0.2.1:1"
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Content-Encoding", "gzip")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("gzip expansion status = %d, want %d", response.Code, http.StatusRequestEntityTooLarge)
	}
	tooMany := make([]usage.Event, maxBatchEvents+1)
	for index := range tooMany {
		tooMany[index] = securityEvent("event-" + string(rune(index+1000)))
	}
	if response := postBatch(t, server, tooMany, "secret", "192.0.2.1:1"); response.Code != http.StatusBadRequest {
		t.Fatalf("event count status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestSettingsAuthCSRFAndTrustedProxyBoundary(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := NewWithConfig(store, Config{IngestToken: "secret", DashboardToken: "dashboard", TrustedProxyCIDRs: []string{"10.0.0.0/8"}})
	if err != nil {
		t.Fatal(err)
	}
	unauthorized := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated settings status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}
	trusted := httptest.NewRequest(http.MethodGet, "/settings", nil)
	trusted.RemoteAddr = "10.0.0.2:1234"
	trusted.Header.Set("X-Forwarded-User", "alice")
	trustedResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(trustedResponse, trusted)
	if trustedResponse.Code != http.StatusOK || !strings.Contains(trustedResponse.Body.String(), "csrf_token") {
		t.Fatalf("trusted proxy settings response = %d: %s", trustedResponse.Code, trustedResponse.Body.String())
	}
	untrusted := httptest.NewRequest(http.MethodGet, "/settings", nil)
	untrusted.RemoteAddr = "192.0.2.2:1234"
	untrusted.Header.Set("X-Forwarded-User", "alice")
	untrustedResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(untrustedResponse, untrusted)
	if untrustedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("untrusted forwarded identity status = %d, want %d", untrustedResponse.Code, http.StatusUnauthorized)
	}
	form := strings.NewReader("model_identity=a&model_alias=b")
	post := httptest.NewRequest(http.MethodPost, "http://example.com/settings/aliases", form)
	post.SetBasicAuth("tokemon", "dashboard")
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(postResponse, post)
	if postResponse.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d, want %d", postResponse.Code, http.StatusForbidden)
	}
}

// Dashboard password authentication (port of the pre-merge auth design onto
// main's token/CSRF model): a bcrypt hash generated by `tokemon passwd`
// accepts a username/password login as an alternative to the raw dashboard
// token, without weakening any other boundary.
func TestDashboardPasswordAuthRequiresPairedConfig(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	hash, err := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		config Config
	}{
		{"username without hash", Config{IngestToken: "secret", DashboardUsername: "kim"}},
		{"hash without username", Config{IngestToken: "secret", DashboardPasswordHash: hash}},
		{"invalid hash", Config{IngestToken: "secret", DashboardUsername: "kim", DashboardPasswordHash: []byte("not-bcrypt")}},
	}
	for _, test := range cases {
		if _, err := NewWithConfig(store, test.config); err == nil {
			t.Errorf("%s: NewWithConfig accepted an invalid password auth configuration", test.name)
		}
	}
	if _, err := NewWithConfig(store, Config{IngestToken: "secret", DashboardUsername: "kim", DashboardPasswordHash: hash}); err != nil {
		t.Fatalf("valid password auth configuration rejected: %v", err)
	}
}

func TestDashboardPasswordAuthGrantsAndLimitsAccess(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	hash, err := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewWithConfig(store, Config{IngestToken: "secret", DashboardUsername: "kim", DashboardPasswordHash: hash})
	if err != nil {
		t.Fatal(err)
	}

	requests := []struct {
		name   string
		setup  func(*http.Request)
		path   string
		status int
	}{
		{"correct password", func(r *http.Request) { r.SetBasicAuth("kim", "hunter2") }, "/", http.StatusOK},
		{"wrong password", func(r *http.Request) { r.SetBasicAuth("kim", "wrong") }, "/", http.StatusUnauthorized},
		{"unknown username", func(r *http.Request) { r.SetBasicAuth("other", "hunter2") }, "/", http.StatusUnauthorized},
		{"dashboard token still works", func(r *http.Request) { r.SetBasicAuth("tokemon", "secret") }, "/", http.StatusOK},
		{"bearer dashboard token still works", func(r *http.Request) { r.Header.Set("Authorization", "Bearer secret") }, "/", http.StatusOK},
		{"settings page with password", func(r *http.Request) { r.SetBasicAuth("kim", "hunter2") }, "/settings", http.StatusOK},
	}
	for _, test := range requests {
		request := httptest.NewRequest(http.MethodGet, test.path, nil)
		test.setup(request)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != test.status {
			t.Errorf("%s: status = %d, want %d", test.name, response.Code, test.status)
		}
	}

	// CSRF is preserved: the password-authenticated settings page carries the
	// token, and a password-authenticated write without it is rejected.
	settings := httptest.NewRequest(http.MethodGet, "/settings", nil)
	settings.SetBasicAuth("kim", "hunter2")
	settingsResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(settingsResponse, settings)
	if settingsResponse.Code != http.StatusOK || !strings.Contains(settingsResponse.Body.String(), "csrf_token") {
		t.Fatalf("password-auth settings response = %d: %s", settingsResponse.Code, settingsResponse.Body.String())
	}
	save := httptest.NewRequest(http.MethodPost, "/settings/aliases", strings.NewReader("model_identity=x&model_alias=y"))
	save.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	save.SetBasicAuth("kim", "hunter2")
	saveResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(saveResponse, save)
	if saveResponse.Code == http.StatusOK {
		t.Fatal("password-authenticated settings write without CSRF was accepted")
	}
}

// dashboardLoginRequest sends an explicitly-credentialed dashboard request
// from a fixed remote address so throttling can be exercised per source.
func dashboardLoginRequest(t *testing.T, server *Server, remote, target string, authorize func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.RemoteAddr = remote
	if authorize != nil {
		authorize(request)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func TestDashboardPasswordAuthorizedKeepsUsernameAndPrecedence(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	if dashboardPasswordAuthorized(request, "kim", hash) {
		t.Fatal("request without credentials authorized")
	}
	request.SetBasicAuth("kim", "hunter2")
	if !dashboardPasswordAuthorized(request, "kim", hash) {
		t.Fatal("matching username and password not authorized")
	}
	request.SetBasicAuth("other", "hunter2")
	if dashboardPasswordAuthorized(request, "kim", hash) {
		t.Fatal("wrong username authorized")
	}
	request.SetBasicAuth("kim", "wrong")
	if dashboardPasswordAuthorized(request, "kim", hash) {
		t.Fatal("wrong password authorized")
	}
	if dashboardPasswordAuthorized(request, "", hash) || dashboardPasswordAuthorized(request, "kim", nil) {
		t.Fatal("unconfigured password account authorized")
	}
}

func TestDashboardLoginRateLimitBlocksAndEscalates(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := NewWithConfig(store, Config{IngestToken: "secret", DashboardToken: "dashboard"})
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Unix(1700000000, 0)
	server.loginThrottle.now = func() time.Time { return clock }
	remote := "192.0.2.50:1234"
	fail := func() *httptest.ResponseRecorder {
		return dashboardLoginRequest(t, server, remote, "/", func(r *http.Request) { r.SetBasicAuth("tokemon", "wrong") })
	}

	// Failures below the threshold are allowed through as 401s, and valid
	// credentials still authenticate (also resetting the partial count).
	for attempt := 0; attempt < 2; attempt++ {
		if response := fail(); response.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d status = %d, want %d", attempt+1, response.Code, http.StatusUnauthorized)
		}
	}
	if response := dashboardLoginRequest(t, server, remote, "/", func(r *http.Request) { r.Header.Set("Authorization", "Bearer dashboard") }); response.Code != http.StatusOK {
		t.Fatalf("valid bearer token status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}

	// A fresh run of failures: every attempt up to the threshold gets a 401,
	// the next one is blocked with a Retry-After for the base lockout.
	for attempt := 0; attempt < loginFailuresAllowed; attempt++ {
		if response := fail(); response.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d status = %d, want %d", attempt+1, response.Code, http.StatusUnauthorized)
		}
	}
	blocked := fail()
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("locked status = %d, want %d: %s", blocked.Code, http.StatusTooManyRequests, blocked.Body.String())
	}
	if retryAfter := blocked.Header().Get("Retry-After"); retryAfter != "15" {
		t.Fatalf("Retry-After = %q, want %d", retryAfter, int(loginBaseLockout/time.Second))
	}

	// A different remote is unaffected, and boundaries outside the dashboard
	// (health, token-authenticated ingest) stay open for the locked remote.
	if response := dashboardLoginRequest(t, server, "192.0.2.51:1234", "/", func(r *http.Request) { r.SetBasicAuth("tokemon", "wrong") }); response.Code != http.StatusUnauthorized {
		t.Fatalf("other remote status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if response := dashboardLoginRequest(t, server, remote, "/healthz", nil); response.Code != http.StatusOK {
		t.Fatalf("health while dashboard-locked status = %d: %s", response.Code, response.Body.String())
	}
	if response := postBatch(t, server, []usage.Event{securityEvent("throttled")}, "secret", remote); response.Code != http.StatusOK {
		t.Fatalf("ingest while dashboard-locked status = %d: %s", response.Code, response.Body.String())
	}

	// After the lockout expires the next failure escalates the backoff.
	clock = clock.Add(loginBaseLockout + time.Second)
	if response := fail(); response.Code != http.StatusUnauthorized {
		t.Fatalf("post-lockout failure status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	blocked = fail()
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("escalated status = %d, want %d", blocked.Code, http.StatusTooManyRequests)
	}
	if retryAfter := blocked.Header().Get("Retry-After"); retryAfter != "30" {
		t.Fatalf("escalated Retry-After = %q, want 30", retryAfter)
	}
}

func TestDashboardLoginRateLimitResetsOnSuccess(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	hash, err := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewWithConfig(store, Config{IngestToken: "secret", DashboardUsername: "kim", DashboardPasswordHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Unix(1700000000, 0)
	server.loginThrottle.now = func() time.Time { return clock }
	remote := "192.0.2.60:1234"
	fail := func() *httptest.ResponseRecorder {
		return dashboardLoginRequest(t, server, remote, "/", func(r *http.Request) { r.SetBasicAuth("kim", "wrong") })
	}

	for attempt := 0; attempt < loginFailuresAllowed-1; attempt++ {
		if response := fail(); response.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d status = %d, want %d", attempt+1, response.Code, http.StatusUnauthorized)
		}
	}
	if response := dashboardLoginRequest(t, server, remote, "/", func(r *http.Request) { r.SetBasicAuth("kim", "hunter2") }); response.Code != http.StatusOK {
		t.Fatalf("correct sign-in after failures status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}

	// The success cleared the record: a full run of failures is allowed
	// through again before the remote is locked out.
	for attempt := 0; attempt < loginFailuresAllowed; attempt++ {
		if response := fail(); response.Code != http.StatusUnauthorized {
			t.Fatalf("post-reset failure %d status = %d, want %d", attempt+1, response.Code, http.StatusUnauthorized)
		}
	}
	if blocked := fail(); blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("post-reset locked status = %d, want %d", blocked.Code, http.StatusTooManyRequests)
	}

	// A quiet window also resets the count: the next failure starts a fresh
	// burst instead of inheriting the lockout.
	clock = clock.Add(loginFailureWindow + time.Minute)
	if response := fail(); response.Code != http.StatusUnauthorized {
		t.Fatalf("quiet-window failure status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestLoginLockoutEscalationIsBounded(t *testing.T) {
	tests := []struct {
		failures int
		want     time.Duration
	}{
		{failures: 1, want: 0},
		{failures: loginFailuresAllowed - 1, want: 0},
		{failures: loginFailuresAllowed, want: 15 * time.Second},
		{failures: loginFailuresAllowed + 1, want: 30 * time.Second},
		{failures: loginFailuresAllowed + 2, want: 60 * time.Second},
		{failures: loginFailuresAllowed + 3, want: 120 * time.Second},
		{failures: loginFailuresAllowed + 4, want: 240 * time.Second},
		{failures: loginFailuresAllowed + 5, want: 480 * time.Second},
		{failures: loginFailuresAllowed + 6, want: loginMaxLockout},
		{failures: loginFailuresAllowed + 100, want: loginMaxLockout},
	}
	for _, test := range tests {
		if got := loginLockout(test.failures); got != test.want {
			t.Fatalf("loginLockout(%d) = %s, want %s", test.failures, got, test.want)
		}
	}
}

// TestDashboardAuthValidSessionBeatsLoginThrottle pins the gate order behind
// issue #40: the session cookie is validated before the login throttle, so a
// locked-out IP with a live session is served; requests that present
// credential material stay throttled, and an expired cookie from a locked-out
// IP takes the anonymous path instead of a 429.
func TestDashboardAuthValidSessionBeatsLoginThrottle(t *testing.T) {
	const (
		lockedRemote   = "192.0.2.7:4444"
		unlockedRemote = "192.0.2.99:5555"
	)
	// Mint a cookie bound to the case server's own session key and
	// credential fingerprint: live or expired.
	liveSession := func(t *testing.T, server *Server) string {
		t.Helper()
		fingerprint := credentialFingerprint(server.config.DashboardUsername, server.config.DashboardPasswordHash)
		value, err := mintSessionCookieValue(server.sessionSecret, time.Now(), time.Now().Add(sessionTTL), fingerprint)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	expiredSession := func(t *testing.T, server *Server) string {
		t.Helper()
		fingerprint := credentialFingerprint(server.config.DashboardUsername, server.config.DashboardPasswordHash)
		value, err := mintSessionCookieValue(server.sessionSecret, time.Now().Add(-2*sessionTTL), time.Now().Add(-sessionTTL), fingerprint)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}

	tests := []struct {
		name       string
		remote     string
		session    func(t *testing.T, server *Server) string // nil = no cookie
		basicUser  string
		basicPass  string
		wantStatus int
		check      func(t *testing.T, response *httptest.ResponseRecorder)
	}{
		{
			// The bug: this used to 429 because the throttle ran before the
			// cookie check.
			name:       "valid session from a locked-out IP is served, not throttled",
			remote:     lockedRemote,
			session:    liveSession,
			wantStatus: http.StatusOK,
			check: func(t *testing.T, response *httptest.ResponseRecorder) {
				if response.Header().Get("Retry-After") != "" {
					t.Error("session-authenticated response carries Retry-After")
				}
				if !strings.Contains(response.Body.String(), "Log out") {
					t.Error("authenticated page missing the logout affordance")
				}
			},
		},
		{
			name:       "rejected credentials from a locked-out IP stay throttled",
			remote:     lockedRemote,
			basicUser:  "ops",
			basicPass:  "wrong",
			wantStatus: http.StatusTooManyRequests,
			check: func(t *testing.T, response *httptest.ResponseRecorder) {
				if response.Header().Get("Retry-After") == "" {
					t.Error("locked response missing Retry-After")
				}
			},
		},
		{
			name:       "expired cookie from a locked-out IP is anonymous, not throttled",
			remote:     lockedRemote,
			session:    expiredSession,
			wantStatus: http.StatusSeeOther,
			check: func(t *testing.T, response *httptest.ResponseRecorder) {
				if location := response.Header().Get("Location"); !strings.HasPrefix(location, "/login") {
					t.Errorf("anonymous redirect = %q, want /login", location)
				}
				if response.Header().Get("Retry-After") != "" {
					t.Error("anonymous path carries Retry-After")
				}
				if cleared := findCookie(response, sessionCookieName); cleared == nil || cleared.MaxAge > 0 {
					t.Error("expired cookie was not cleared")
				}
			},
		},
		{
			name:       "valid credentials from an unlocked IP still work",
			remote:     unlockedRemote,
			basicUser:  "ops",
			basicPass:  "hunter2",
			wantStatus: http.StatusOK,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// A fresh server per case: recordSuccess on one path must not
			// unlock the remote another case relies on being locked out.
			server, _, _ := newLoginTestServer(t)
			clock := time.Unix(1700000000, 0)
			server.loginThrottle.now = func() time.Time { return clock }
			lockRemoteOut(t, server, lockedRemote)

			request := httptest.NewRequest(http.MethodGet, "/analytics", nil)
			request.RemoteAddr = test.remote
			request.Header.Set("Accept", "text/html,application/xhtml+xml")
			if test.session != nil {
				request.AddCookie(sessionCookie(test.session(t, server), 0, false))
			}
			if test.basicUser != "" {
				request.SetBasicAuth(test.basicUser, test.basicPass)
			}
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.wantStatus, response.Body.String())
			}
			if test.check != nil {
				test.check(t, response)
			}
		})
	}
}

// lockRemoteOut records enough rejected credential attempts through the real
// handler path to engage the lockout for remote's address.
func lockRemoteOut(t *testing.T, server *Server, remote string) {
	t.Helper()
	for attempt := 0; attempt < loginFailuresAllowed; attempt++ {
		response := dashboardLoginRequest(t, server, remote, "/", func(r *http.Request) { r.SetBasicAuth("ops", "wrong") })
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("lockout failure %d status = %d, want %d", attempt+1, response.Code, http.StatusUnauthorized)
		}
	}
}
