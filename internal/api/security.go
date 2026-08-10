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
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
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
	AllowLoopbackDev  bool
	TrustedProxyCIDRs []string
}

func (c Config) normalized() Config {
	if c.DashboardToken == "" {
		c.DashboardToken = c.IngestToken
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
	return false
}

func (s *Server) dashboardAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.dashboardAuthorized(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="tokemon"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "dashboard authentication required"})
			return
		}
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
