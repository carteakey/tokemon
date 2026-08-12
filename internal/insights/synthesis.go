// Package insights contains the optional, privacy-bounded insight synthesis
// client. It deliberately has no dependency on Tokemon's database or usage
// packages: callers must first turn their aggregate card into Input.
package insights

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	defaultBaseURL = "https://api.openai.com"
	defaultModel   = "gpt-5.6-luna"

	defaultTimeout  = 15 * time.Second
	defaultCacheTTL = 5 * time.Minute

	maxPeriodLabel   = 120
	maxTimezone      = 64
	maxScopeLabels   = 12
	maxScopeLabel    = 80
	maxEvidence      = 5
	maxEvidenceID    = 96
	maxCategory      = 80
	maxObservation   = 800
	maxBasis         = 800
	maxQualityLevel  = 32
	maxQualityNote   = 240
	maxOutputTitle   = 120
	maxOutputSummary = 600
	maxHTTPBodyBytes = 1 << 20
)

// ScopeLabel is a generalized bucket count. Label must be a caller-redacted
// category (for example, "top categories"), never a dimension name or an
// identifier. Count is intentionally the only numeric field in a scope.
type ScopeLabel struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// Stable scope labels intentionally describe only the shape of an aggregate
// card. A caller must map any page-level filter or dimension name to one of
// these labels before constructing Input.
const (
	ScopeOverall        = "overall"
	ScopeFilteredView   = "filtered view"
	ScopeCurrentPeriod  = "current period"
	ScopeSelectedPeriod = "selected period"
	ScopeAllActivity    = "all activity"
	ScopeTopCategories  = "top categories"
	ScopeOther          = "other"
	ScopeUnknown        = "unknown"
)

var allowedScopeLabels = map[string]struct{}{
	ScopeOverall: {}, ScopeFilteredView: {}, ScopeCurrentPeriod: {},
	ScopeSelectedPeriod: {}, ScopeAllActivity: {}, ScopeTopCategories: {},
	ScopeOther: {}, ScopeUnknown: {},
}

// IsAggregateScopeLabel reports whether label is safe to use in Input. It is
// case-insensitive and ignores surrounding whitespace.
func IsAggregateScopeLabel(label string) bool {
	_, ok := allowedScopeLabels[strings.ToLower(strings.TrimSpace(label))]
	return ok
}

// DataQuality describes the reliability of the aggregate card. It contains
// no source-level metadata.
type DataQuality struct {
	Level string `json:"level"`
	Note  string `json:"note,omitempty"`
}

// Evidence is a caller-redacted aggregate observation. ID is the only value
// that may be returned by the model as a reference. Observation and Basis may
// contain pre-formatted aggregate values, but must not contain raw records.
type Evidence struct {
	ID          string `json:"id"`
	Category    string `json:"category"`
	Observation string `json:"observation"`
	Basis       string `json:"basis,omitempty"`
}

// Input is the aggregate-only card sent to the optional synthesizer. It is
// independent of database and usage types by design.
type Input struct {
	PeriodLabel string       `json:"period_label"`
	Timezone    string       `json:"timezone"`
	ScopeLabels []ScopeLabel `json:"scope_labels,omitempty"`
	DataQuality DataQuality  `json:"data_quality"`
	Evidence    []Evidence   `json:"evidence"`
}

// Output is the intentionally small structured response. EvidenceIDs must be
// a subset of IDs supplied in Input.Evidence.
type Output struct {
	Title       string   `json:"title"`
	Summary     string   `json:"summary"`
	EvidenceIDs []string `json:"evidence_ids"`
}

// Status describes whether a synthesis operation was performed and, when it
// was not, why the caller should use its deterministic fallback card.
type Status string

const (
	StatusDisabled    Status = "disabled"
	StatusReady       Status = "ready"
	StatusCached      Status = "cached"
	StatusTimeout     Status = "timeout"
	StatusError       Status = "error"
	StatusRefused     Status = "refused"
	StatusInvalid     Status = "invalid"
	StatusUnavailable Status = "unavailable"
)

// Config controls the OpenAI Responses API client. A blank API key disables
// network synthesis. HTTPClient is useful for tests and for callers with
// custom transport policy; it is never persisted by this package.
type Config struct {
	BaseURL    string
	Model      string
	APIKey     string
	Timeout    time.Duration
	CacheTTL   time.Duration
	HTTPClient *http.Client
}

// Synthesizer is the narrow seam used by API/dashboard callers. A fake can
// implement it without importing any database or HTTP details.
type Synthesizer interface {
	Synthesize(context.Context, Input) (Output, error)
}

// StatusProvider is implemented by Client for callers that want to expose
// optional-feature state. Keeping it separate lets API tests inject a tiny
// fake that only implements Synthesize.
type StatusProvider interface {
	Status() Status
}

// Result carries an operation status while retaining the simple Output/error
// interface for callers. It is available through Client.SynthesizeResult.
type Result struct {
	Output   Output
	Status   Status
	Cached   bool
	Fallback bool
	Err      error
}

// SynthesisResult is a descriptive alias for Result.
type SynthesisResult = Result

// SynthesisError preserves a machine-readable fallback signal while allowing
// errors.Is/errors.As to inspect the underlying cause.
type SynthesisError struct {
	Status Status
	Err    error
}

func (e *SynthesisError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return string(e.Status)
	}
	return fmt.Sprintf("insight synthesis %s: %v", e.Status, e.Err)
}

func (e *SynthesisError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

var (
	// ErrDisabled means no API call was attempted because the API key is blank.
	ErrDisabled = errors.New("insight synthesis is disabled")
	// ErrInvalidInput means the aggregate card failed privacy or bound checks.
	ErrInvalidInput = errors.New("invalid aggregate insight input")
	// ErrInvalidOutput means the model response did not satisfy the strict
	// structured-output contract or referenced unsupported data.
	ErrInvalidOutput = errors.New("invalid insight synthesis output")
	// ErrRefused means the provider explicitly declined to generate an answer.
	ErrRefused = errors.New("insight synthesis was refused")
)

// StatusOf returns the operation status encoded by err. Ordinary errors are
// classified as StatusError; nil is StatusReady.
func StatusOf(err error) Status {
	if err == nil {
		return StatusReady
	}
	var synthesisErr *SynthesisError
	if errors.As(err, &synthesisErr) && synthesisErr.Status != "" {
		return synthesisErr.Status
	}
	return StatusError
}

// Client is an in-memory, TTL-cached Responses API synthesizer. No input,
// output, or API credential is written to disk.
type Client struct {
	baseURL    string
	model      string
	apiKey     string
	timeout    time.Duration
	cacheTTL   time.Duration
	httpClient *http.Client

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	output  Output
	expires time.Time
}

// New constructs an optional synthesizer. It does not make a network request.
func New(cfg Config) *Client {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = defaultBaseURL
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		model = defaultModel
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ttl := cfg.CacheTTL
	if ttl == 0 {
		ttl = defaultCacheTTL
	}
	return &Client{
		baseURL:    base,
		model:      model,
		apiKey:     strings.TrimSpace(cfg.APIKey),
		timeout:    timeout,
		cacheTTL:   ttl,
		httpClient: cfg.HTTPClient,
		cache:      make(map[string]cacheEntry),
	}
}

// Disabled constructs a no-network synthesizer. It is equivalent to New with
// an empty API key and is useful when wiring optional features.
func Disabled() *Client { return New(Config{}) }

// Status reports configuration state without making a request.
func (c *Client) Status() Status {
	if c == nil || c.apiKey == "" {
		return StatusDisabled
	}
	if c.invalidConfiguration() {
		return StatusUnavailable
	}
	return StatusReady
}

func (c *Client) invalidConfiguration() bool {
	if c == nil || strings.TrimSpace(c.baseURL) == "" || strings.TrimSpace(c.model) == "" {
		return true
	}
	u, err := url.Parse(c.baseURL)
	return err != nil || u.Scheme == "" || u.Host == ""
}

// ClearCache removes all in-memory entries. It is primarily useful after a
// caller changes its aggregate card policy during a process lifetime.
func (c *Client) ClearCache() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.cache = make(map[string]cacheEntry)
	c.mu.Unlock()
}

// Synthesize validates and, when enabled, sends an aggregate card to the
// Responses API. Errors are typed with a status so callers can render a safe
// deterministic fallback.
func (c *Client) Synthesize(ctx context.Context, input Input) (Output, error) {
	result := c.SynthesizeResult(ctx, input)
	return result.Output, result.Err
}

// SynthesizeResult is the status-aware form of Synthesize.
func (c *Client) SynthesizeResult(ctx context.Context, input Input) Result {
	if c == nil || c.apiKey == "" {
		return Result{Status: StatusDisabled, Fallback: true, Err: &SynthesisError{Status: StatusDisabled, Err: ErrDisabled}}
	}
	if c.invalidConfiguration() {
		return Result{Status: StatusUnavailable, Fallback: true, Err: &SynthesisError{Status: StatusUnavailable, Err: errors.New("invalid insights client configuration")}}
	}
	sanitized, err := ValidateInput(input)
	if err != nil {
		return Result{Status: StatusInvalid, Fallback: true, Err: &SynthesisError{Status: StatusInvalid, Err: err}}
	}
	key, err := inputHash(sanitized)
	if err != nil {
		return Result{Status: StatusInvalid, Fallback: true, Err: &SynthesisError{Status: StatusInvalid, Err: err}}
	}
	if output, ok := c.cached(key, time.Now()); ok {
		return Result{Output: output, Status: StatusCached, Cached: true}
	}

	output, status, err := c.request(ctx, sanitized)
	if err != nil {
		return Result{Status: status, Fallback: true, Err: &SynthesisError{Status: status, Err: err}}
	}
	if c.cacheTTL > 0 {
		c.putCache(key, output, time.Now().Add(c.cacheTTL))
	}
	return Result{Output: output, Status: StatusReady}
}

func (c *Client) cached(key string, now time.Time) (Output, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.cache[key]
	if !ok {
		return Output{}, false
	}
	if !now.Before(entry.expires) {
		delete(c.cache, key)
		return Output{}, false
	}
	return cloneOutput(entry.output), true
}

func (c *Client) putCache(key string, output Output, expires time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		c.cache = make(map[string]cacheEntry)
	}
	c.cache[key] = cacheEntry{output: cloneOutput(output), expires: expires}
}

func cloneOutput(output Output) Output {
	output.EvidenceIDs = append([]string(nil), output.EvidenceIDs...)
	return output
}

func inputHash(input Input) (string, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func (c *Client) request(ctx context.Context, input Input) (Output, Status, error) {
	body, err := buildRequestBody(c.model, input)
	if err != nil {
		return Output{}, StatusInvalid, err
	}
	client := c.httpClient
	if client == nil {
		client = &http.Client{Timeout: c.timeout}
	}
	requestCtx := ctx
	if requestCtx == nil {
		requestCtx = context.Background()
	}
	var cancel context.CancelFunc
	requestCtx, cancel = context.WithTimeout(requestCtx, c.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.baseURL+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		return Output{}, StatusUnavailable, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	response, err := client.Do(request)
	if err != nil {
		if errors.Is(requestCtx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			return Output{}, StatusTimeout, err
		}
		return Output{}, StatusError, err
	}
	defer response.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, maxHTTPBodyBytes+1))
	if readErr != nil {
		return Output{}, StatusError, readErr
	}
	if len(responseBody) > maxHTTPBodyBytes {
		return Output{}, StatusError, errors.New("responses API body exceeds size limit")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return Output{}, StatusError, fmt.Errorf("responses API returned %s: %s", response.Status, compactBody(responseBody))
	}
	output, status, parseErr := parseResponse(responseBody, input.Evidence)
	if parseErr != nil {
		return Output{}, status, parseErr
	}
	return output, StatusReady, nil
}

func compactBody(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 512 {
		text = text[:512] + "…"
	}
	return text
}

// outputSchema is intentionally narrow. No free-form metadata, source
// identifiers, or numeric fields are offered to the model.
var outputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"properties": map[string]any{
		"title": map[string]any{
			"type":      "string",
			"maxLength": maxOutputTitle,
		},
		"summary": map[string]any{
			"type":      "string",
			"maxLength": maxOutputSummary,
		},
		"evidence_ids": map[string]any{
			"type":     "array",
			"maxItems": maxEvidence,
			"items": map[string]any{
				"type":      "string",
				"maxLength": maxEvidenceID,
			},
		},
	},
	"required": []string{"title", "summary", "evidence_ids"},
}

type responseRequest struct {
	Model           string       `json:"model"`
	Store           bool         `json:"store"`
	Input           string       `json:"input"`
	MaxOutputTokens int          `json:"max_output_tokens"`
	Text            responseText `json:"text"`
}

type responseText struct {
	Format responseFormat `json:"format"`
}

type responseFormat struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Strict      bool           `json:"strict"`
	Schema      map[string]any `json:"schema"`
}

type promptCard struct {
	PeriodLabel string           `json:"period_label"`
	Timezone    string           `json:"timezone"`
	ScopeLabels []promptScope    `json:"scope_labels,omitempty"`
	DataQuality promptQuality    `json:"data_quality"`
	Evidence    []promptEvidence `json:"evidence"`
}

type promptScope struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

type promptQuality struct {
	Level string `json:"level"`
	Note  string `json:"note,omitempty"`
}

type promptEvidence struct {
	ID          string `json:"id"`
	Category    string `json:"category"`
	Observation string `json:"observation"`
	Basis       string `json:"basis,omitempty"`
}

func buildRequestBody(model string, input Input) ([]byte, error) {
	card := promptCard{
		PeriodLabel: input.PeriodLabel,
		Timezone:    input.Timezone,
		DataQuality: promptQuality{Level: input.DataQuality.Level, Note: input.DataQuality.Note},
		Evidence:    make([]promptEvidence, len(input.Evidence)),
	}
	if len(input.ScopeLabels) > 0 {
		card.ScopeLabels = make([]promptScope, len(input.ScopeLabels))
		for index, scope := range input.ScopeLabels {
			card.ScopeLabels[index] = promptScope{Label: scope.Label, Count: scope.Count}
		}
	}
	for index, evidence := range input.Evidence {
		card.Evidence[index] = promptEvidence{
			ID: evidence.ID, Category: evidence.Category,
			Observation: evidence.Observation, Basis: evidence.Basis,
		}
	}
	cardJSON, err := json.Marshal(card)
	if err != nil {
		return nil, err
	}
	inputText := "Create a concise aggregate insight. Return only JSON matching the supplied schema. Use only supplied evidence references and do not invent quantities.\nAggregate card:\n" + string(cardJSON)
	body := responseRequest{
		Model: model, Store: false, Input: inputText,
		MaxOutputTokens: 300,
		Text: responseText{Format: responseFormat{
			Type:        "json_schema",
			Name:        "tokemon_insight",
			Description: "A short aggregate-only insight with references to supplied evidence IDs.",
			Strict:      true,
			Schema:      outputSchema,
		}},
	}
	return json.Marshal(body)
}

var sensitiveKeyPattern = regexp.MustCompile(`(?i)(?:^|["'{}\s,])(?:session|project|machine|provider|model|tool|timestamp|prompt|response|repository|repo|path|hostname|username|email|api[_-]?key|secret|credential|token[_-]?id)(?:["'{}\s_-]*:)`)
var numberPattern = regexp.MustCompile(`(?i)\b\d[\d,]*(?:\.\d+)?(?:%|[kmgt])?\b`)

func rejectSensitiveKey(name, value string) error {
	if sensitiveKeyPattern.MatchString(value) {
		return fmt.Errorf("%s contains a suspicious sensitive key", name)
	}
	// A raw JSON object/array is almost certainly a record rather than a
	// caller-redacted card sentence. Evidence remains free-form text, but this
	// cheap guard catches the common accidental marshal of an event.
	trimmed := strings.TrimSpace(value)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return fmt.Errorf("%s must not contain raw structured records", name)
	}
	return nil
}

func validateText(name, value string, max int, required bool) (string, error) {
	value = strings.TrimSpace(value)
	if required && value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	if len([]rune(value)) > max {
		return "", fmt.Errorf("%s exceeds %d characters", name, max)
	}
	for _, character := range value {
		if character == '\u0000' || (character < 0x20 && character != '\n' && character != '\t') {
			return "", fmt.Errorf("%s contains control characters", name)
		}
	}
	if err := rejectSensitiveKey(name, value); err != nil {
		return "", err
	}
	return value, nil
}

// ValidateInput returns a trimmed, bounded copy suitable for hashing and
// transport. Callers can use it independently to validate a card before
// deciding whether to enable network synthesis.
func ValidateInput(input Input) (Input, error) {
	// Clone slice-backed fields before trimming or normalizing them so caller
	// state remains unchanged and the cache key represents only this request.
	input.ScopeLabels = append([]ScopeLabel(nil), input.ScopeLabels...)
	input.Evidence = append([]Evidence(nil), input.Evidence...)
	var err error
	input.PeriodLabel, err = validateText("period_label", input.PeriodLabel, maxPeriodLabel, true)
	if err != nil {
		return Input{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	input.Timezone, err = validateText("timezone", input.Timezone, maxTimezone, true)
	if err != nil {
		return Input{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if len(input.ScopeLabels) > maxScopeLabels {
		return Input{}, fmt.Errorf("%w: scope labels exceed %d", ErrInvalidInput, maxScopeLabels)
	}
	for index := range input.ScopeLabels {
		scope := &input.ScopeLabels[index]
		scope.Label, err = validateText(fmt.Sprintf("scope_labels[%d].label", index), scope.Label, maxScopeLabel, true)
		if err != nil {
			return Input{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
		scope.Label = strings.ToLower(scope.Label)
		if _, allowed := allowedScopeLabels[scope.Label]; !allowed {
			return Input{}, fmt.Errorf("%w: scope_labels[%d].label %q is not an aggregate scope label", ErrInvalidInput, index, scope.Label)
		}
		if scope.Count < 0 || scope.Count > 1_000_000_000 {
			return Input{}, fmt.Errorf("%w: scope_labels[%d].count out of bounds", ErrInvalidInput, index)
		}
	}
	input.DataQuality.Level, err = validateText("data_quality.level", input.DataQuality.Level, maxQualityLevel, true)
	if err != nil {
		return Input{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	input.DataQuality.Note, err = validateText("data_quality.note", input.DataQuality.Note, maxQualityNote, false)
	if err != nil {
		return Input{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if len(input.Evidence) > maxEvidence {
		return Input{}, fmt.Errorf("%w: evidence exceeds %d items", ErrInvalidInput, maxEvidence)
	}
	seenIDs := make(map[string]struct{}, len(input.Evidence))
	for index := range input.Evidence {
		evidence := &input.Evidence[index]
		evidence.ID, err = validateEvidenceID(evidence.ID)
		if err != nil {
			return Input{}, fmt.Errorf("%w: evidence[%d].id: %v", ErrInvalidInput, index, err)
		}
		if _, exists := seenIDs[evidence.ID]; exists {
			return Input{}, fmt.Errorf("%w: duplicate evidence id %q", ErrInvalidInput, evidence.ID)
		}
		seenIDs[evidence.ID] = struct{}{}
		evidence.Category, err = validateText(fmt.Sprintf("evidence[%d].category", index), evidence.Category, maxCategory, true)
		if err != nil {
			return Input{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
		evidence.Observation, err = validateText(fmt.Sprintf("evidence[%d].observation", index), evidence.Observation, maxObservation, true)
		if err != nil {
			return Input{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
		evidence.Basis, err = validateText(fmt.Sprintf("evidence[%d].basis", index), evidence.Basis, maxBasis, false)
		if err != nil {
			return Input{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
	}
	return input, nil
}

func validateEvidenceID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("is required")
	}
	if len([]rune(value)) > maxEvidenceID {
		return "", fmt.Errorf("exceeds %d characters", maxEvidenceID)
	}
	for _, character := range value {
		if character < 0x21 || character > 0x7e || character == '"' || character == '\\' {
			return "", errors.New("must be printable opaque text")
		}
	}
	return value, nil
}

type responseEnvelope struct {
	Status     string            `json:"status"`
	OutputText string            `json:"output_text"`
	Output     []json.RawMessage `json:"output"`
	Error      json.RawMessage   `json:"error"`
}

func parseResponse(body []byte, evidence []Evidence) (Output, Status, error) {
	var envelope responseEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Output{}, StatusInvalid, fmt.Errorf("%w: response is not JSON: %v", ErrInvalidOutput, err)
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		return Output{}, StatusError, fmt.Errorf("responses API returned an error: %s", compactBody(envelope.Error))
	}
	status := strings.ToLower(strings.TrimSpace(envelope.Status))
	if status == "failed" || status == "cancelled" {
		return Output{}, StatusError, fmt.Errorf("responses API response status is %q", status)
	}
	if status == "incomplete" || status == "in_progress" || status == "queued" {
		return Output{}, StatusError, fmt.Errorf("responses API response is not complete: %q", status)
	}

	if refusal := findRefusal(body); refusal != "" {
		return Output{}, StatusRefused, fmt.Errorf("%w: %s", ErrRefused, refusal)
	}
	text := strings.TrimSpace(envelope.OutputText)
	if text == "" {
		text = findOutputText(envelope.Output)
	}
	if text == "" {
		return Output{}, StatusInvalid, fmt.Errorf("%w: response did not contain output text", ErrInvalidOutput)
	}
	text = stripJSONFence(text)
	var rawOutput map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &rawOutput); err != nil {
		return Output{}, StatusInvalid, fmt.Errorf("%w: %v", ErrInvalidOutput, err)
	}
	for _, key := range []string{"title", "summary", "evidence_ids"} {
		rawValue, present := rawOutput[key]
		if !present || bytes.Equal(bytes.TrimSpace(rawValue), []byte("null")) {
			return Output{}, StatusInvalid, fmt.Errorf("%w: missing required field %q", ErrInvalidOutput, key)
		}
	}
	var output Output
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&output); err != nil {
		return Output{}, StatusInvalid, fmt.Errorf("%w: %v", ErrInvalidOutput, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Output{}, StatusInvalid, fmt.Errorf("%w: response contains trailing JSON", ErrInvalidOutput)
		}
		return Output{}, StatusInvalid, fmt.Errorf("%w: malformed trailing JSON: %v", ErrInvalidOutput, err)
	}
	if err := ValidateOutput(output, evidence); err != nil {
		return Output{}, StatusInvalid, err
	}
	return output, StatusReady, nil
}

func stripJSONFence(text string) string {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") && strings.HasSuffix(text, "```") {
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, "```json"), "```"))
		if strings.HasPrefix(text, "```") {
			text = strings.TrimSpace(strings.TrimPrefix(text, "```"))
		}
	}
	return text
}

func findOutputText(items []json.RawMessage) string {
	for _, raw := range items {
		var item struct {
			Type    string            `json:"type"`
			Text    string            `json:"text"`
			Content []json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		if strings.EqualFold(item.Type, "output_text") && strings.TrimSpace(item.Text) != "" {
			return item.Text
		}
		if text := findOutputText(item.Content); text != "" {
			return text
		}
	}
	return ""
}

func findRefusal(body []byte) string {
	var value any
	if json.Unmarshal(body, &value) != nil {
		return ""
	}
	return findRefusalValue(value)
}

func findRefusalValue(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		if typeName, ok := typed["type"].(string); ok && strings.EqualFold(typeName, "refusal") {
			if refusal, ok := typed["refusal"].(string); ok {
				return strings.TrimSpace(refusal)
			}
			return "provider refusal"
		}
		for key, child := range typed {
			if strings.EqualFold(key, "refusal") {
				if refusal, ok := child.(string); ok && strings.TrimSpace(refusal) != "" {
					return strings.TrimSpace(refusal)
				}
			}
			if refusal := findRefusalValue(child); refusal != "" {
				return refusal
			}
		}
	case []any:
		for _, child := range typed {
			if refusal := findRefusalValue(child); refusal != "" {
				return refusal
			}
		}
	}
	return ""
}

// ValidateOutput applies the same bounds and privacy checks locally to a
// decoded model result. It is exported so fakes and deterministic fallbacks
// can share the evidence-reference contract.
func ValidateOutput(output Output, evidence []Evidence) error {
	var err error
	output.Title, err = validateText("title", output.Title, maxOutputTitle, true)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidOutput, err)
	}
	output.Summary, err = validateText("summary", output.Summary, maxOutputSummary, true)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidOutput, err)
	}
	if len(output.EvidenceIDs) > maxEvidence {
		return fmt.Errorf("%w: too many evidence references", ErrInvalidOutput)
	}
	known := make(map[string]struct{}, len(evidence))
	evidenceNumbers := make(map[string]struct{})
	for _, item := range evidence {
		known[item.ID] = struct{}{}
		for _, number := range numberPattern.FindAllString(item.Observation+" "+item.Basis, -1) {
			evidenceNumbers[normalizeNumber(number)] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, len(output.EvidenceIDs))
	for index, id := range output.EvidenceIDs {
		id, err = validateEvidenceID(id)
		if err != nil {
			return fmt.Errorf("%w: evidence reference %d: %v", ErrInvalidOutput, index, err)
		}
		if _, ok := known[id]; !ok {
			return fmt.Errorf("%w: evidence reference %q was not supplied", ErrInvalidOutput, id)
		}
		if _, ok := seen[id]; ok {
			return fmt.Errorf("%w: duplicate evidence reference %q", ErrInvalidOutput, id)
		}
		seen[id] = struct{}{}
	}
	for _, number := range numberPattern.FindAllString(output.Title+" "+output.Summary, -1) {
		if _, ok := evidenceNumbers[normalizeNumber(number)]; !ok {
			return fmt.Errorf("%w: unsupported numeric literal %q", ErrInvalidOutput, number)
		}
	}
	return nil
}

func normalizeNumber(value string) string {
	value = strings.ToLower(strings.ReplaceAll(value, ",", ""))
	return value
}

// Ensure Client remains assignable to the public seam.
var _ Synthesizer = (*Client)(nil)
var _ StatusProvider = (*Client)(nil)
