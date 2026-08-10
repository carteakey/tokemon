package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
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
