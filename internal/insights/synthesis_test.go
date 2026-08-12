package insights

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testInput() Input {
	return Input{
		PeriodLabel: "Last 7 days",
		Timezone:    "America/Toronto",
		ScopeLabels: []ScopeLabel{{Label: ScopeFilteredView, Count: 3}},
		DataQuality: DataQuality{Level: "complete", Note: "aggregate-only"},
		Evidence: []Evidence{
			{ID: "ev-alpha", Category: "trend", Observation: "Usage rose to 10% from the prior period.", Basis: "10% known aggregate"},
			{ID: "ev-beta", Category: "coverage", Observation: "Three buckets were complete."},
		},
	}
}

func responseJSON(output string) string {
	encoded, _ := json.Marshal(output)
	return `{"id":"resp_test","status":"completed","output_text":` + string(encoded) + `}`
}

func TestResponsesRequestIsAggregateOnlyAndStructured(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Method != http.MethodPost || request.URL.Path != "/v1/responses" {
			t.Errorf("request = %s %s, want POST /v1/responses", request.Method, request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer test-secret" {
			t.Errorf("authorization = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body["store"] != false {
			t.Errorf("store = %#v, want false", body["store"])
		}
		if body["model"] != "test-model" {
			t.Errorf("model = %#v", body["model"])
		}
		if body["max_output_tokens"] != float64(300) {
			t.Errorf("max_output_tokens = %#v", body["max_output_tokens"])
		}
		if _, present := body["api_key"]; present {
			t.Error("API key appeared in request body")
		}
		text, ok := body["text"].(map[string]any)
		if !ok {
			t.Fatalf("text = %#v", body["text"])
		}
		format, ok := text["format"].(map[string]any)
		if !ok || format["type"] != "json_schema" || format["strict"] != true {
			t.Fatalf("text.format = %#v", text["format"])
		}
		if _, ok := format["schema"].(map[string]any); !ok {
			t.Fatalf("schema missing from text.format: %#v", format)
		}
		inputText, ok := body["input"].(string)
		if !ok {
			t.Fatalf("input = %#v", body["input"])
		}
		for _, forbidden := range []string{"session_id", "project_id", "machine_id", "provider_id", "model_id", "tool_id", "timestamp", "raw_events", "api_key"} {
			if strings.Contains(inputText, forbidden) {
				t.Errorf("forbidden identifier %q in input text: %s", forbidden, inputText)
			}
		}
		if strings.Contains(inputText, "test-secret") {
			t.Error("API key appeared in input text")
		}
		if !strings.Contains(inputText, `"scope_labels"`) || !strings.Contains(inputText, `"count":3`) {
			t.Errorf("generalized scope count missing from input: %s", inputText)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(responseJSON(`{"title":"Usage pulse","summary":"Usage rose to 10%.","evidence_ids":["ev-alpha"]}`)))
	}))
	defer server.Close()

	client := New(Config{BaseURL: server.URL, Model: "test-model", APIKey: "test-secret", Timeout: time.Second})
	output, err := client.Synthesize(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Synthesize() error = %v", err)
	}
	if output.Title != "Usage pulse" || len(output.EvidenceIDs) != 1 || output.EvidenceIDs[0] != "ev-alpha" {
		t.Fatalf("output = %+v", output)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestSynthesizeUsesTTLCachedSanitizedInput(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(responseJSON(`{"title":"Cached","summary":"Stable result","evidence_ids":[]}`)))
	}))
	defer server.Close()
	client := New(Config{BaseURL: server.URL, APIKey: "key", CacheTTL: time.Minute})
	input := testInput()
	input.Evidence = nil
	first, err := client.Synthesize(context.Background(), input)
	if err != nil {
		t.Fatalf("first Synthesize() error = %v", err)
	}
	second, err := client.Synthesize(context.Background(), input)
	if err != nil {
		t.Fatalf("second Synthesize() error = %v", err)
	}
	if first.Title != second.Title || calls.Load() != 1 {
		t.Fatalf("first=%+v second=%+v calls=%d", first, second, calls.Load())
	}
	result := client.SynthesizeResult(context.Background(), input)
	if result.Status != StatusCached || !result.Cached || result.Err != nil {
		t.Fatalf("cached result = %+v", result)
	}
	client.ClearCache()
	_, err = client.Synthesize(context.Background(), input)
	if err != nil || calls.Load() != 2 {
		t.Fatalf("after ClearCache error=%v calls=%d", err, calls.Load())
	}
}

func TestDisabledDoesNotCallNetwork(t *testing.T) {
	client := Disabled()
	output, err := client.Synthesize(context.Background(), testInput())
	if !errors.Is(err, ErrDisabled) || output.Title != "" || output.Summary != "" || len(output.EvidenceIDs) != 0 {
		t.Fatalf("output=%+v error=%v", output, err)
	}
	if client.Status() != StatusDisabled || StatusOf(err) != StatusDisabled {
		t.Fatalf("status=%s statusOf=%s", client.Status(), StatusOf(err))
	}
}

func TestTimeoutAndHTTPErrorSignals(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer slow.Close()
	client := New(Config{BaseURL: slow.URL, APIKey: "key", Timeout: 10 * time.Millisecond})
	_, err := client.Synthesize(context.Background(), testInput())
	if StatusOf(err) != StatusTimeout {
		t.Fatalf("timeout status = %s, error=%v", StatusOf(err), err)
	}

	errorServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Error(response, `{"error":"nope"}`, http.StatusBadGateway)
	}))
	defer errorServer.Close()
	client = New(Config{BaseURL: errorServer.URL, APIKey: "key"})
	_, err = client.Synthesize(context.Background(), testInput())
	if StatusOf(err) != StatusError {
		t.Fatalf("HTTP error status = %s, error=%v", StatusOf(err), err)
	}
}

func TestRefusalAndInvalidSchemaSignals(t *testing.T) {
	refusalServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"not safe"}]}]}`))
	}))
	defer refusalServer.Close()
	client := New(Config{BaseURL: refusalServer.URL, APIKey: "key"})
	_, err := client.Synthesize(context.Background(), testInput())
	if !errors.Is(err, ErrRefused) || StatusOf(err) != StatusRefused {
		t.Fatalf("refusal status=%s error=%v", StatusOf(err), err)
	}

	invalidServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(responseJSON(`{"title":"Bad","summary":"Unsupported 99","evidence_ids":["unknown"]}`)))
	}))
	defer invalidServer.Close()
	client = New(Config{BaseURL: invalidServer.URL, APIKey: "key"})
	_, err = client.Synthesize(context.Background(), testInput())
	if !errors.Is(err, ErrInvalidOutput) || StatusOf(err) != StatusInvalid {
		t.Fatalf("invalid status=%s error=%v", StatusOf(err), err)
	}
}

func TestValidateInputAndOutputPrivacyBounds(t *testing.T) {
	input := testInput()
	originalLabel := input.ScopeLabels[0].Label
	if _, err := ValidateInput(input); err != nil {
		t.Fatalf("baseline input error=%v", err)
	}
	if input.ScopeLabels[0].Label != originalLabel {
		t.Fatalf("ValidateInput mutated caller scope label to %q", input.ScopeLabels[0].Label)
	}
	input = testInput()
	input.ScopeLabels[0].Label = "project-alpha"
	if _, err := ValidateInput(input); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("scope label error=%v", err)
	}
	input = testInput()
	input.Evidence[0].Observation = `{"session_id":"secret"}`
	if _, err := ValidateInput(input); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("sensitive key error=%v", err)
	}
	input = testInput()
	input.Evidence[0].Observation = strings.Repeat("x", maxObservation+1)
	if _, err := ValidateInput(input); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("overlong error=%v", err)
	}

	evidence := []Evidence{{ID: "ev", Category: "trend", Observation: "10 tokens"}}
	if err := ValidateOutput(Output{Title: "10 tokens", Summary: "Okay", EvidenceIDs: []string{"ev"}}, evidence); err != nil {
		t.Fatalf("supported number rejected: %v", err)
	}
	if err := ValidateOutput(Output{Title: "11 tokens", Summary: "No", EvidenceIDs: []string{"ev"}}, evidence); !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("unsupported number error=%v", err)
	}
	if err := ValidateOutput(Output{Title: "No", Summary: "Okay", EvidenceIDs: []string{"missing"}}, evidence); !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("unknown reference error=%v", err)
	}
}

func TestNestedOutputTextAndJSONFence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		body := map[string]any{
			"status": "completed",
			"output": []any{map[string]any{
				"type": "message",
				"content": []any{map[string]any{
					"type": "output_text",
					"text": "```json\n{\"title\":\"Nested\",\"summary\":\"No numbers\",\"evidence_ids\":[]}\n```",
				}},
			}},
		}
		_ = json.NewEncoder(response).Encode(body)
	}))
	defer server.Close()
	client := New(Config{BaseURL: server.URL, APIKey: "key"})
	output, err := client.Synthesize(context.Background(), Input{PeriodLabel: "Week", Timezone: "UTC", DataQuality: DataQuality{Level: "unknown"}})
	if err != nil || output.Title != "Nested" {
		t.Fatalf("output=%+v error=%v", output, err)
	}
}
