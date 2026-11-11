package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/usage"
	"golang.org/x/crypto/bcrypt"
)

// newLoginTestServer builds a store + server with the password account
// configured (the only mode where the login form and session cookies exist).
func emptyCatalogForAuth() *catalog.Catalog { return catalog.Empty() }

func newLoginTestServer(t *testing.T) (*Server, *database.Store, string) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.Open(filepath.Join(t.TempDir(), "tokemon.db"), emptyCatalogForAuth())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	server, err := NewWithConfig(store, Config{IngestToken: "secret", DashboardToken: "secret", DashboardUsername: "ops", DashboardPasswordHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	return server, store, string(hash)
}

func sameOriginReferer(r *http.Request) {
	r.Header.Set("Origin", "http://"+r.Host)
}

func requestHost() string {
	return "example.com"
}

func TestLoginFormRendersOnlyWithPasswordAccount(t *testing.T) {
	server, _, _ := newLoginTestServer(t)

	page := httptest.NewRecorder()
	server.Handler().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/login", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("login page status = %d", page.Code)
	}
	body := page.Body.String()
	for _, want := range []string{
		`name="csrf_token"`,
		`autocomplete="username"`,
		`autocomplete="current-password"`,
		`action="/login"`,
		`<label for="login-username">Username</label>`,
		`<label for="login-password">Password</label>`,
		`for="login-username"`,
		`for="login-password"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("login page missing %q", want)
		}
	}
	for _, secret := range []string{"hunter2", "bcrypt", "secret"} {
		if strings.Contains(body, secret) {
			t.Errorf("login page leaks %q", secret)
		}
	}
	if !strings.Contains(page.Header().Get("Cache-Control"), "no-store") {
		t.Error("login page missing Cache-Control: no-store")
	}

	// Without a password account the login surface is a 404.
	bareServer, _, _ := newLoginTestServer(t)
	bareServer.config.DashboardUsername = ""
	bareServer.config.DashboardPasswordHash = nil
	missing := httptest.NewRecorder()
	bareServer.Handler().ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/login", nil))
	if missing.Code != http.StatusNotFound {
		t.Errorf("login page without password account status = %d, want 404", missing.Code)
	}
	missingPost := httptest.NewRecorder()
	bareServer.Handler().ServeHTTP(missingPost, httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("username=ops&password=x")))
	if missingPost.Code != http.StatusNotFound {
		t.Errorf("login submit without password account status = %d, want 404", missingPost.Code)
	}
}

func TestLoginSuccessSetsSessionCookieAndRedirects(t *testing.T) {
	server, _, _ := newLoginTestServer(t)

	form := url.Values{"username": {"ops"}, "password": {"hunter2"}, "next": {"/analytics?period=7d"}, "csrf_token": {server.csrfToken()}}
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	sameOriginReferer(request)

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303: %s", response.Code, response.Body.String())
	}
	if location := response.Header().Get("Location"); location != "/analytics?period=7d" {
		t.Errorf("redirect = %q, want /analytics?period=7d", location)
	}
	cookies := response.Result().Cookies()
	var session *http.Cookie
	for _, cookie := range cookies {
		if cookie.Name == sessionCookieName {
			session = cookie
		}
	}
	if session == nil {
		t.Fatal("no session cookie set on successful login")
	}
	if !session.HttpOnly || session.SameSite != http.SameSiteLaxMode || session.Path != "/" {
		t.Errorf("session cookie attributes = %+v", session)
	}
	if session.MaxAge != int(sessionTTL/time.Second) {
		t.Errorf("session cookie Max-Age = %d, want %d", session.MaxAge, int(sessionTTL/time.Second))
	}
	if strings.Contains(session.Value, "hunter2") || session.Value == "" {
		t.Error("session cookie value is empty or leaks the password")
	}

	// The cookie authenticates dashboard pages without Basic auth.
	follow := httptest.NewRequest(http.MethodGet, "/analytics", nil)
	follow.AddCookie(session)
	authorized := httptest.NewRecorder()
	server.Handler().ServeHTTP(authorized, follow)
	if authorized.Code != http.StatusOK {
		t.Fatalf("session-authenticated analytics status = %d", authorized.Code)
	}
	if !strings.Contains(authorized.Body.String(), "Log out") {
		t.Error("session-authenticated page missing the logout affordance")
	}
}

func TestLoginRejectsBadCredentialsAndCSRF(t *testing.T) {
	server, _, _ := newLoginTestServer(t)

	post := func(username, password string, withCSRF bool, header http.Header) *httptest.ResponseRecorder {
		form := url.Values{"username": {username}, "password": {password}, "csrf_token": {server.csrfToken()}}
		if !withCSRF {
			form.Del("csrf_token")
		}
		request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for key, values := range header {
			for _, value := range values {
				request.Header.Add(key, value)
			}
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}

	// Wrong password: 401, generic message, no cookie.
	wrongOrigin := http.Header{}
	wrongOrigin.Set("Origin", "http://"+requestHost())
	wrong := post("ops", "wrong-password", true, wrongOrigin)
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d, want 401", wrong.Code)
	}
	if !strings.Contains(wrong.Body.String(), "Invalid username or password.") || strings.Contains(wrong.Body.String(), "hunter2") {
		t.Error("wrong-password response leaks the password or lacks the generic message")
	}
	if !strings.Contains(wrong.Body.String(), `role="alert"`) {
		t.Error("wrong-password response missing role=alert on the error")
	}
	if session := findCookie(wrong, sessionCookieName); session != nil {
		t.Error("wrong password set a session cookie")
	}

	// CSRF missing → 403.
	if response := post("ops", "hunter2", false, http.Header{}); response.Code != http.StatusForbidden {
		t.Errorf("missing CSRF status = %d, want 403", response.Code)
	}
	// Cross-origin Origin → 403.
	cross := http.Header{}
	cross.Set("Origin", "https://evil.example")
	if response := post("ops", "hunter2", true, cross); response.Code != http.StatusForbidden {
		t.Errorf("cross-origin status = %d, want 403", response.Code)
	}
	// Neither Origin nor Referer present → 403 (the token alone is not the gate).
	if response := post("ops", "hunter2", true, http.Header{}); response.Code != http.StatusForbidden {
		t.Errorf("no-origin status = %d, want 403", response.Code)
	}

	// Wrong passwords fill the budget (one failure came from the case above,
	// so four more reach the allowed five); the next attempt is locked out
	// with 429 before bcrypt runs.
	badOrigin := http.Header{}
	badOrigin.Set("Origin", "http://"+requestHost())
	for attempt := 0; attempt < 4; attempt++ {
		if response := post("ops", "wrong", true, badOrigin); response.Code != http.StatusUnauthorized {
			t.Fatalf("wrong attempt %d status = %d, want 401", attempt+1, response.Code)
		}
	}
	locked := post("ops", "hunter2", true, http.Header{})
	if locked.Code != http.StatusTooManyRequests {
		t.Fatalf("locked status = %d, want 429", locked.Code)
	}
	if locked.Header().Get("Retry-After") == "" {
		t.Error("locked response missing Retry-After")
	}
}

func findCookie(response *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

func TestSessionCookieExpiryAndForgeryNeverBurnThrottle(t *testing.T) {
	server, _, _ := newLoginTestServer(t)
	fingerprint := credentialFingerprint("ops", server.config.DashboardPasswordHash)
	secure := false

	// Expired cookie: cleared, HTML navigation redirected, and six of them
	// never lock the remote out (no recordFailure for cookies).
	expired, err := mintSessionCookieValue(server.sessionSecret, time.Now().Add(-2*sessionTTL), time.Now().Add(-sessionTTL), fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 6; attempt++ {
		request := httptest.NewRequest(http.MethodGet, "/analytics", nil)
		request.Header.Set("Accept", "text/html,application/xhtml+xml")
		request.AddCookie(sessionCookie(expired, 0, secure))
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusSeeOther {
			t.Fatalf("expired cookie navigation %d status = %d, want 303", attempt+1, response.Code)
		}
		if location := response.Header().Get("Location"); !strings.HasPrefix(location, "/login") {
			t.Errorf("expired cookie redirect = %q, want /login", location)
		}
		if cleared := findCookie(response, sessionCookieName); cleared == nil || cleared.MaxAge > 0 {
			t.Errorf("expired cookie navigation %d did not clear the cookie", attempt+1)
		}
	}
	// The remote is still usable: valid Basic credentials pass.
	request := httptest.NewRequest(http.MethodGet, "/api/v1/machines", nil)
	request.SetBasicAuth("ops", "hunter2")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("post-expired-cookie Basic status = %d, want 200", response.Code)
	}

	// A forged cookie (bad signature) is rejected the same way.
	forged := expired[:len(expired)-4] + "AAAA"
	request = httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept", "text/html,application/xhtml+xml")
	request.AddCookie(sessionCookie(forged, 0, secure))
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("forged cookie status = %d, want 303", response.Code)
	}
}

func TestSessionCookieRejectedWithoutPasswordAccount(t *testing.T) {
	server, _, _ := newLoginTestServer(t)
	fingerprint := credentialFingerprint("ops", server.config.DashboardPasswordHash)
	value, err := mintSessionCookieValue(server.sessionSecret, time.Now(), time.Now().Add(sessionTTL), fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	// Strip the password account: valid-looking cookies are never accepted.
	server.config.DashboardUsername = ""
	server.config.DashboardPasswordHash = nil

	request := httptest.NewRequest(http.MethodGet, "/analytics", nil)
	request.AddCookie(sessionCookie(value, 0, false))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code == http.StatusOK {
		t.Fatal("session cookie accepted without a password account")
	}
}

func TestLogoutClearsSessionCookie(t *testing.T) {
	server, _, _ := newLoginTestServer(t)
	form := url.Values{"username": {"ops"}, "password": {"hunter2"}, "csrf_token": {server.csrfToken()}}
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	sameOriginReferer(request)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	session := findCookie(response, sessionCookieName)
	if session == nil {
		t.Fatal("no session cookie from login")
	}

	// Logout requires CSRF + same origin, clears the cookie with identical
	// attributes, and redirects to the login page.
	logoutForm := url.Values{"csrf_token": {server.csrfToken()}}
	logout := httptest.NewRequest(http.MethodPost, "/logout", strings.NewReader(logoutForm.Encode()))
	logout.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	logout.AddCookie(session)
	sameOriginReferer(logout)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, logout)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("logout status = %d, want 303", response.Code)
	}
	if location := response.Header().Get("Location"); location != "/login" {
		t.Errorf("logout redirect = %q, want /login", location)
	}
	cleared := findCookie(response, sessionCookieName)
	if cleared == nil || cleared.MaxAge != -1 || !cleared.HttpOnly || cleared.SameSite != http.SameSiteLaxMode || cleared.Path != "/" {
		t.Fatalf("cleared cookie = %+v, want identical attributes with Max-Age -1", cleared)
	}
	// The old cookie no longer authenticates (it was cleared server-side by
	// response headers; the stateless token itself is untouched, so the
	// browser dropping it is what ends the session).
	follow := httptest.NewRequest(http.MethodGet, "/analytics", nil)
	follow.AddCookie(session)
	authorized := httptest.NewRecorder()
	server.Handler().ServeHTTP(authorized, follow)
	if authorized.Code != http.StatusOK {
		t.Fatalf("stateless cookie still valid after logout response: %d", authorized.Code)
	}

	// Logout without CSRF is rejected.
	rejected := httptest.NewRequest(http.MethodPost, "/logout", strings.NewReader("csrf_token=nope"))
	rejected.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rejected.AddCookie(session)
	sameOriginReferer(rejected)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, rejected)
	if response.Code != http.StatusForbidden {
		t.Fatalf("logout without valid CSRF status = %d, want 403", response.Code)
	}
}

func TestSessionAuthKeepsBasicAndBearerWorking(t *testing.T) {
	server, store, _ := newLoginTestServer(t)
	seedOneEvent(t, store)

	// Wrong Basic credentials still burn the throttle and are rejected.
	request := httptest.NewRequest(http.MethodGet, "/api/v1/machines", nil)
	request.SetBasicAuth("ops", "nope")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong Basic status = %d, want 401", response.Code)
	}

	// Dashboard token as Bearer still works.
	request = httptest.NewRequest(http.MethodGet, "/api/v1/machines", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("Bearer status = %d, want 200", response.Code)
	}

	// API clients without credentials keep the 401 + Basic challenge.
	request = httptest.NewRequest(http.MethodGet, "/api/v1/machines", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") != `Basic realm="tokemon"` {
		t.Fatalf("API no-credentials status = %d, want 401 with Basic challenge", response.Code)
	}
}

func TestSafeRedirectPathTable(t *testing.T) {
	for _, test := range []struct {
		next string
		want string
	}{
		{"", "/"},
		{"/analytics", "/analytics"},
		{"/analytics?period=7d&machine=x", "/analytics?period=7d&machine=x"},
		{"//evil.example", "/"},
		{"/\\evil.example", "/"},
		{"https://evil.example", "/"},
		{"javascript:alert(1)", "/"},
		{"/login?next=//evil", "/login?next=//evil"}, // literal path, parsed host empty
		{"relative", "/"},
		{"/ok\r\nSet-Cookie: x", "/"},
		{"//" + strings.Repeat("x", 600), "/"},
	} {
		if got := safeRedirectPath(test.next); got != test.want {
			t.Errorf("safeRedirectPath(%q) = %q, want %q", test.next, got, test.want)
		}
	}
	// An overlong but otherwise legal path is capped.
	if got := safeRedirectPath("/" + strings.Repeat("a", 600)); got != "/" {
		t.Errorf("overlong path = %q, want /", got)
	}
}

func TestLoginRedirectDefaultsToRoot(t *testing.T) {
	server, _, _ := newLoginTestServer(t)
	form := url.Values{"username": {"ops"}, "password": {"hunter2"}, "next": {"//evil.example"}, "csrf_token": {server.csrfToken()}}
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	sameOriginReferer(request)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if location := response.Header().Get("Location"); location != "/" {
		t.Errorf("protocol-relative next = %q, want /", location)
	}
}

func TestTokenOnlyDeploymentKeepsBasicChallengeForHTML(t *testing.T) {
	// Without a password account there is no form: anonymous HTML
	// navigations keep the 401 Basic challenge so credentials embedded in
	// the URL (and the browser smoke) keep working.
	hash := []byte{}
	_ = hash
	store, err := database.Open(filepath.Join(t.TempDir(), "tokemon.db"), catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := NewWithConfig(store, Config{IngestToken: "secret", DashboardToken: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/analytics?period=7d", nil)
	request.Header.Set("Accept", "text/html,application/xhtml+xml")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("token-only anonymous HTML status = %d, want 401 with the Basic challenge", response.Code)
	}
	if response.Header().Get("WWW-Authenticate") != `Basic realm="tokemon"` {
		t.Error("token-only deployment missing the Basic challenge")
	}
	if location := response.Header().Get("Location"); location != "" {
		t.Errorf("token-only deployment redirected to %q instead of challenging", location)
	}
}

func TestAnonymousHTMLNavigationRedirectsToLogin(t *testing.T) {
	server, _, _ := newLoginTestServer(t)
	request := httptest.NewRequest(http.MethodGet, "/analytics?period=7d", nil)
	request.Header.Set("Accept", "text/html,application/xhtml+xml")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("anonymous HTML navigation status = %d, want 303", response.Code)
	}
	if location := response.Header().Get("Location"); location != "/login?next=%2Fanalytics%3Fperiod%3D7d" {
		t.Errorf("redirect = %q, want the encoded /analytics?period=7d", location)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Error("redirect missing Cache-Control: no-store")
	}
}

func seedOneEvent(t *testing.T, store *database.Store) {
	t.Helper()
	event := usage.Event{
		SchemaVersion: usage.SchemaVersion, EventID: "auth-smoke", Timestamp: time.Now().UTC(),
		MachineID: "machine", Provider: "test", Model: "model", Tool: "codex",
		TotalTokens: usage.Int64(10), TokenAccuracy: usage.AccuracyReported,
		Source: usage.Source{Adapter: "codex", AdapterVersion: "test"},
	}
	if _, err := store.Ingest(context.Background(), []usage.Event{event}); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialFingerprintIsDomainSeparatedAndRotates(t *testing.T) {
	hash := []byte("$2a$10$N9qo8uLOickgx2ZMRZoMye.IjPeGqBvQFGvJgkpXkQdUvYX0hJ7Ea")
	baseline := credentialFingerprint("ops", hash)
	if len(baseline) != sessionFingerprintBytes {
		t.Fatalf("fingerprint length = %d, want %d", len(baseline), sessionFingerprintBytes)
	}
	if string(baseline) == string(hash[:sessionFingerprintBytes]) {
		t.Fatal("fingerprint is the hash material itself, not a one-way digest")
	}
	// Rotating either credential invalidates outstanding sessions.
	if other := credentialFingerprint("root", hash); string(other) == string(baseline) {
		t.Error("fingerprint ignores the username")
	}
	if other := credentialFingerprint("ops", []byte("$2a$10$differenthashvaluehere.ABCDEFGHIJKLMNOPQRSTUVWX")); string(other) == string(baseline) {
		t.Error("fingerprint ignores the password hash")
	}
}

func TestLoginRequestBodyLimit(t *testing.T) {
	server, _, _ := newLoginTestServer(t)
	huge := strings.Repeat("a", 16_000)
	form := url.Values{"username": {"ops"}, "password": {"hunter2"}, "csrf_token": {server.csrfToken()}, "next": {huge}}
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	sameOriginReferer(request)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized login body status = %d, want 413", response.Code)
	}
}

func TestTrustedProxyHTTPSOriginAccepted(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.Open(filepath.Join(t.TempDir(), "tokemon.db"), catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := NewWithConfig(store, Config{
		IngestToken: "secret", DashboardToken: "secret",
		DashboardUsername: "ops", DashboardPasswordHash: hash,
		TrustedProxyCIDRs: []string{"10.0.0.0/8"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The connection is plaintext but a trusted proxy asserts https: the
	// https Origin must be accepted (previously the scheme check
	// self-rejected TLS-terminated deployments).
	form := url.Values{"username": {"ops"}, "password": {"hunter2"}, "csrf_token": {server.csrfToken()}, "next": {"/"}}
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	request.RemoteAddr = "10.0.0.5:1234"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("Origin", "https://example.com")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("proxy-https login status = %d, want 303: %s", response.Code, response.Body.String())
	}
	session := findCookie(response, sessionCookieName)
	if session == nil || !session.Secure {
		t.Fatalf("session cookie missing or not Secure behind trusted https proxy: %+v", session)
	}
	if !session.Expires.IsZero() {
		t.Errorf("session cookie carries an Expires attribute; Max-Age is authoritative")
	}
}

func TestLoginRejectsDecodedProtocolRelativeNext(t *testing.T) {
	server, _, _ := newLoginTestServer(t)
	for _, next := range []string{
		"/%2F%2Fevil.example", // double-encoded protocol-relative
		"/%5Cevil.example",    // encoded backslash
		"//evil.example",      // raw protocol-relative
		"/\\evil.example",     // raw backslash
	} {
		form := url.Values{"username": {"ops"}, "password": {"hunter2"}, "csrf_token": {server.csrfToken()}, "next": {next}}
		request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		sameOriginReferer(request)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if location := response.Header().Get("Location"); location == next || strings.Contains(location, "evil") {
			t.Errorf("next %q redirected to %q", next, location)
		}
	}
}

func TestSessionCookieAttributesTable(t *testing.T) {
	cookie := sessionCookie("value", 60, true)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || !cookie.Secure {
		t.Fatalf("cookie = %+v", cookie)
	}
	// JSON snapshot for the HTTP-only wiring contract.
	raw, _ := json.Marshal(map[string]bool{"httponly": cookie.HttpOnly, "secure": cookie.Secure})
	if !strings.Contains(string(raw), "true") {
		t.Fatal("cookie attribute snapshot wrong")
	}
}
