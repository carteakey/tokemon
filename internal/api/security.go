package api

import (
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"golang.org/x/crypto/bcrypt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	errInvalidGzip  = errors.New("invalid gzip body")
	errBodyRead     = errors.New("could not read request body")
	errBodyTooLarge = errors.New("request body too large")
	errInvalidJSON  = errors.New("invalid JSON body")
)

const (
	maxCompressedBodyBytes   int64 = 10 << 20
	maxDecompressedBodyBytes int64 = 8 << 20
	maxBatchBodyBytes        int64 = 4 << 20
	maxBatchEvents                 = 1000
)

// Config defines the trust boundaries for the HTTP server. An empty ingest
// token is never accepted on a non-loopback listener. DashboardToken falls
// back to IngestToken when omitted.
type Config struct {
	IngestToken       string
	DashboardToken    string
	DashboardUsername string
	// DashboardPasswordHash holds a bcrypt hash (see `tokemon passwd`). When
	// it and DashboardUsername are both set, the dashboard accepts that
	// username and password as an alternative to the raw dashboard token.
	DashboardPasswordHash []byte
	// DashboardTokenFallback records that the dashboard token came from the
	// documented ingest-token default rather than explicit configuration.
	DashboardTokenFallback bool
	AllowLoopbackDev       bool
	TrustedProxyCIDRs      []string
	Logger                 *slog.Logger
	Metrics                *Metrics
	RequestTimeout         time.Duration
	HealthTimeout          time.Duration
	HeartbeatTimeout       time.Duration
	IngestTimeout          time.Duration
	StaleAgentAfter        time.Duration
}

const (
	DefaultRequestTimeout   = 30 * time.Second
	DefaultHealthTimeout    = 5 * time.Second
	DefaultHeartbeatTimeout = 15 * time.Second
	DefaultIngestTimeout    = 30 * time.Second
	DefaultStaleAgentAfter  = 5 * time.Minute
)

func (c Config) normalized() Config {
	if c.DashboardToken == "" {
		c.DashboardToken = c.IngestToken
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Metrics == nil {
		c.Metrics = NewMetrics()
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = DefaultRequestTimeout
	}
	if c.HealthTimeout <= 0 {
		c.HealthTimeout = DefaultHealthTimeout
	}
	if c.HeartbeatTimeout <= 0 {
		c.HeartbeatTimeout = DefaultHeartbeatTimeout
	}
	if c.IngestTimeout <= 0 {
		c.IngestTimeout = DefaultIngestTimeout
	}
	if c.StaleAgentAfter <= 0 {
		c.StaleAgentAfter = DefaultStaleAgentAfter
	}
	return c
}

func parseTrustedProxyCIDRs(values []string) ([]*net.IPNet, error) {
	networks := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			return nil, errors.New("invalid trusted proxy CIDR")
		}
		networks = append(networks, network)
	}
	return networks, nil
}

func loopbackListenAddress(addr string) bool {
	host := strings.TrimSpace(addr)
	if strings.HasPrefix(host, "[") {
		if end := strings.IndexByte(host, ']'); end >= 0 {
			host = host[1:end]
		}
	} else if strings.Count(host, ":") == 1 {
		if parsedHost, _, err := net.SplitHostPort(host); err == nil {
			host = parsedHost
		}
	} else if strings.HasPrefix(host, ":") {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validateDashboardPasswordAuth enforces the pairing and format rules for
// optional dashboard password authentication: username and bcrypt hash are
// configured together or not at all, and the hash must be a usable bcrypt
// digest so a typo fails at startup instead of at every login.
func (c Config) validateDashboardPasswordAuth() error {
	if (c.DashboardUsername == "") != (len(c.DashboardPasswordHash) == 0) {
		return errors.New("TOKEMON_DASHBOARD_USERNAME and TOKEMON_DASHBOARD_PASSWORD_HASH must be set together; generate the hash with: tokemon passwd")
	}
	if len(c.DashboardPasswordHash) == 0 {
		return nil
	}
	if _, err := bcrypt.Cost(c.DashboardPasswordHash); err != nil {
		return errors.New("TOKEMON_DASHBOARD_PASSWORD_HASH is not a bcrypt hash; generate one with: tokemon passwd")
	}
	return nil
}

// dashboardPasswordAuthorized reports whether the request's Basic Auth
// credentials match the configured dashboard username and password hash. The
// username must match exactly (compared in constant time, symmetric with the
// token path); the password is verified against the bcrypt digest.
func dashboardPasswordAuthorized(r *http.Request, username string, hash []byte) bool {
	if username == "" || len(hash) == 0 {
		return false
	}
	basicUsername, basicPassword, ok := r.BasicAuth()
	if !ok || !constantTimeEqual(basicUsername, username) {
		return false
	}
	return bcrypt.CompareHashAndPassword(hash, []byte(basicPassword)) == nil
}

// authSummary describes the effective dashboard authentication posture for the
// settings page: which mode is active and where the dashboard token came from.
func (s *Server) authSummary() (mode, tokenSource string) {
	switch {
	case s.config.DashboardUsername != "" && len(s.config.DashboardPasswordHash) > 0:
		mode = "Password account"
	case s.config.DashboardToken != "":
		mode = "Dashboard token"
	case s.config.AllowLoopbackDev:
		mode = "Loopback development"
	default:
		mode = "Closed"
	}
	switch {
	case s.config.DashboardTokenFallback:
		tokenSource = "ingest token (documented fallback)"
	case s.config.DashboardToken != "":
		tokenSource = "explicit dashboard token"
	default:
		tokenSource = "none configured"
	}
	return mode, tokenSource
}

// ValidateServeConfig prevents an empty ingest token from silently exposing
// a listener beyond loopback. The explicit development flag is required even
// for a loopback bind so production configuration cannot drift into dev mode.
func ValidateServeConfig(addr, ingestToken string, allowLoopbackDev bool) error {
	if strings.TrimSpace(ingestToken) != "" {
		return nil
	}
	if !allowLoopbackDev {
		return errors.New("TOKEMON_INGEST_TOKEN is required; use --dev-loopback only for local development")
	}
	if !loopbackListenAddress(addr) {
		return errors.New("--dev-loopback requires an explicit loopback listen address (127.0.0.1, ::1, or localhost)")
	}
	return nil
}

func newCSRFSecret(token string) ([]byte, error) {
	if token != "" {
		secret := sha256.Sum256([]byte("tokemon/settings/csrf/" + token))
		return secret[:], nil
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, errors.New("could not initialize CSRF protection")
	}
	return secret, nil
}

func (s *Server) csrfToken() string {
	mac := hmac.New(sha256.New, s.csrfSecret)
	_, _ = mac.Write([]byte("settings"))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) validCSRFToken(value string) bool {
	expected := s.csrfToken()
	return subtle.ConstantTimeCompare([]byte(value), []byte(expected)) == 1
}

func requestRemoteIP(r *http.Request) net.IP {
	remote := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}
	return net.ParseIP(strings.Trim(remote, "[]"))
}

func requestIsLoopback(r *http.Request) bool {
	ip := requestRemoteIP(r)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) trustedProxyUser(r *http.Request) string {
	if len(s.trustedProxyNetworks) == 0 {
		return ""
	}
	ip := requestRemoteIP(r)
	if ip == nil {
		return ""
	}
	for _, network := range s.trustedProxyNetworks {
		if network.Contains(ip) {
			// The proxy must strip this header from untrusted clients before
			// forwarding it. Only a non-empty identity is useful here; no
			// identity means the request still needs dashboard credentials.
			return strings.TrimSpace(r.Header.Get("X-Forwarded-User"))
		}
	}
	return ""
}

func constantTimeEqual(provided, expected string) bool {
	providedDigest := sha256.Sum256([]byte(provided))
	expectedDigest := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(providedDigest[:], expectedDigest[:]) == 1
}

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return "", false
	}
	token := strings.TrimPrefix(header, "Bearer ")
	if token == "" || strings.TrimSpace(token) != token || strings.ContainsAny(token, "\r\n") {
		return "", false
	}
	return token, true
}

func validToken(r *http.Request, expected string) bool {
	if expected == "" {
		return false
	}
	headerToken := r.Header.Get("X-Tokemon-Ingest-Token")
	bearer, hasBearer := bearerToken(r)
	if headerToken != "" && hasBearer {
		return constantTimeEqual(headerToken, expected) && constantTimeEqual(bearer, expected)
	}
	if headerToken != "" {
		return constantTimeEqual(headerToken, expected)
	}
	if hasBearer {
		return constantTimeEqual(bearer, expected)
	}
	return false
}

func (s *Server) ingestAuthorized(r *http.Request) bool {
	if s.config.IngestToken == "" {
		return s.config.AllowLoopbackDev && requestIsLoopback(r)
	}
	return validToken(r, s.config.IngestToken)
}

func (s *Server) dashboardAuthorized(r *http.Request) bool {
	if s.config.AllowLoopbackDev && s.config.IngestToken == "" && requestIsLoopback(r) {
		return true
	}
	if s.trustedProxyUser(r) != "" {
		return true
	}
	token := s.config.DashboardToken
	if token == "" {
		return false
	}
	if username, password, ok := r.BasicAuth(); ok && username != "" && constantTimeEqual(password, token) {
		return true
	}
	if provided, ok := bearerToken(r); ok && constantTimeEqual(provided, token) {
		return true
	}
	// Optional password account: configured via `tokemon passwd` plus
	// TOKEMON_DASHBOARD_USERNAME, letting a human sign in without sharing the
	// raw dashboard token. Token credentials keep working.
	if dashboardPasswordAuthorized(r, s.config.DashboardUsername, s.config.DashboardPasswordHash) {
		return true
	}
	return false
}

const (
	// Dashboard login throttling: after loginFailuresAllowed rejected
	// attempts the remote address is locked out, with the lockout doubling
	// per additional failure so sustained guessing decays to about one
	// attempt per loginMaxLockout. A quiet loginFailureWindow (or any
	// successful login) clears the record.
	loginFailuresAllowed  = 5
	loginFailureWindow    = 5 * time.Minute
	loginBaseLockout      = 15 * time.Second
	loginMaxLockout       = 10 * time.Minute
	loginMaxTrackedRemote = 1024
)

// loginAttempt tracks consecutive failed dashboard logins from one remote
// address. firstFailure anchors the burst for observability; the count is
// reset only after a quiet loginFailureWindow.
type loginAttempt struct {
	failures     int
	firstFailure time.Time
	lastFailure  time.Time
	lockedUntil  time.Time
}

// loginThrottle bounds failed dashboard authentication per remote address.
// The table is capped so request floods from many spoofed addresses cannot
// grow it without limit; when full, expired records go first and the least
// recently active remote is evicted otherwise.
type loginThrottle struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]*loginAttempt
}

func newLoginThrottle() *loginThrottle {
	return &loginThrottle{now: time.Now, entries: make(map[string]*loginAttempt)}
}

// loginLockout returns the lockout for a burst that has reached the given
// failure count: the base duration doubled per failure beyond the allowed
// count, capped at loginMaxLockout. Bursts below the threshold are not locked.
func loginLockout(failures int) time.Duration {
	if failures < loginFailuresAllowed {
		return 0
	}
	lockout := loginBaseLockout
	for excess := failures - loginFailuresAllowed; excess > 0 && lockout < loginMaxLockout; excess-- {
		lockout *= 2
	}
	if lockout > loginMaxLockout {
		lockout = loginMaxLockout
	}
	return lockout
}

// check reports whether the remote may attempt dashboard authentication and,
// when it may not, the remaining lockout.
func (l *loginThrottle) check(remote string) (blocked bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	entry, ok := l.entries[remote]
	if !ok || now.Sub(entry.lastFailure) > loginFailureWindow {
		return false, 0
	}
	if remaining := entry.lockedUntil.Sub(now); remaining > 0 {
		return true, remaining
	}
	return false, 0
}

func (l *loginThrottle) recordFailure(remote string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	entry, ok := l.entries[remote]
	if !ok {
		if len(l.entries) >= loginMaxTrackedRemote {
			l.evict(now)
		}
		entry = &loginAttempt{}
		l.entries[remote] = entry
	} else if now.Sub(entry.lastFailure) > loginFailureWindow {
		// Quiet window: the remote starts a fresh burst instead of
		// inheriting an earlier lockout escalation.
		entry.lockedUntil = time.Time{}
		entry.failures = 0
	}
	if entry.failures == 0 {
		entry.firstFailure = now
	}
	entry.failures++
	entry.lastFailure = now
	if entry.failures >= loginFailuresAllowed {
		entry.lockedUntil = now.Add(loginLockout(entry.failures))
	}
}

// recordSuccess clears the remote's record so a legitimate sign-in after
// typos never carries a lockout forward.
func (l *loginThrottle) recordSuccess(remote string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, remote)
}

// evict makes room for a new entry: expired records first, then the least
// recently active remote. Only called while holding l.mu.
func (l *loginThrottle) evict(now time.Time) {
	oldestKey := ""
	oldestFailure := now
	for remote, entry := range l.entries {
		expired := entry.lockedUntil.Before(now) && now.Sub(entry.lastFailure) > loginFailureWindow
		if expired {
			delete(l.entries, remote)
			continue
		}
		if entry.lastFailure.Before(oldestFailure) {
			oldestFailure = entry.lastFailure
			oldestKey = remote
		}
	}
	if len(l.entries) >= loginMaxTrackedRemote && oldestKey != "" {
		delete(l.entries, oldestKey)
	}
}

// loginLimitKey identifies the throttled source. Only the connection's
// address is used — never client-supplied headers — so the key cannot be
// rotated by the requester.
func loginLimitKey(r *http.Request) string {
	if ip := requestRemoteIP(r); ip != nil {
		return ip.String()
	}
	return r.RemoteAddr
}

func (s *Server) dashboardAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote := loginLimitKey(r)
		if blocked, retryAfter := s.loginThrottle.check(remote); blocked {
			seconds := int(retryAfter / time.Second)
			if retryAfter%time.Second != 0 {
				seconds++
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many failed dashboard login attempts; try again later"})
			return
		}
		if !s.dashboardAuthorized(r) {
			s.loginThrottle.recordFailure(remote)
			w.Header().Set("WWW-Authenticate", `Basic realm="tokemon"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "dashboard authentication required"})
			return
		}
		s.loginThrottle.recordSuccess(remote)
		next.ServeHTTP(w, r)
	})
}

func sameOrigin(value string, r *http.Request) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || !strings.EqualFold(parsed.Host, r.Host) {
		return false
	}
	if parsed.Scheme == "" {
		return true
	}
	requestScheme := "http"
	if r.TLS != nil {
		requestScheme = "https"
	}
	return strings.EqualFold(parsed.Scheme, requestScheme)
}

func validSettingsOrigin(r *http.Request) bool {
	for _, header := range []string{"Origin", "Referer"} {
		if value := strings.TrimSpace(r.Header.Get(header)); value != "" && !sameOrigin(value, r) {
			return false
		}
	}
	return true
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, limit int64, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxCompressedBodyBytes)
	var reader io.Reader = r.Body
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		gzipBody, err := gzip.NewReader(r.Body)
		if err != nil {
			return errInvalidGzip
		}
		defer gzipBody.Close()
		reader = gzipBody
	}
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return errBodyRead
	}
	if int64(len(body)) > limit {
		return errBodyTooLarge
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(target); err != nil {
		return errInvalidJSON
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errInvalidJSON
	}
	return nil
}
