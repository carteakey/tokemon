// Package agent contains the local polling client used by the Tokemon binary.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tokemon/tokemon/internal/adapters"
	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/usage"
)

type Client struct {
	ServerURL      string
	Token          string
	HTTPClient     *http.Client
	RequestTimeout time.Duration
}

type batchRequest struct {
	Events []usage.Event `json:"events"`
}

type AdapterHeartbeat struct {
	ID           string                `json:"id"`
	Version      string                `json:"version"`
	Capabilities adapters.Capabilities `json:"capabilities"`
}

type HeartbeatRequest struct {
	MachineID        string             `json:"machine_id"`
	AgentVersion     string             `json:"agent_version"`
	OperatingSystem  string             `json:"operating_system"`
	Architecture     string             `json:"architecture"`
	Adapters         []AdapterHeartbeat `json:"adapters"`
	SourceCount      int                `json:"source_count"`
	SourceErrorCount int                `json:"source_error_count"`
}

const maxIngestBatchBytes = 4 << 20

const DefaultRequestTimeout = 30 * time.Second

func (c Client) requestContext(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := c.RequestTimeout
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	return context.WithTimeout(parent, timeout)
}

func (c Client) Ingest(ctx context.Context, events []usage.Event) (database.IngestResult, error) {
	if strings.TrimSpace(c.ServerURL) == "" {
		return database.IngestResult{}, fmt.Errorf("server URL is required")
	}
	if len(events) == 0 {
		return database.IngestResult{}, fmt.Errorf("events must not be empty")
	}
	// Validate the complete batch before encoding or opening the first HTTP
	// request. A privacy rejection therefore cannot partially upload a batch.
	if err := usage.ValidateOutboundBatch(events); err != nil {
		return database.IngestResult{}, err
	}
	batches, err := splitIngestBatches(events)
	if err != nil {
		return database.IngestResult{}, err
	}
	var combined database.IngestResult
	for index, batch := range batches {
		result, err := c.ingestBatch(ctx, batch)
		if err != nil {
			return database.IngestResult{}, err
		}
		if index == 0 {
			combined.PreviousTotal = result.PreviousTotal
			combined.PreviousStage = result.PreviousStage
		}
		combined.Accepted += result.Accepted
		combined.Duplicates += result.Duplicates
		combined.Rejected += result.Rejected
		combined.Errors = append(combined.Errors, result.Errors...)
		combined.CurrentTotal = result.CurrentTotal
		combined.CurrentStage = result.CurrentStage
		combined.Evolved = combined.Evolved || result.Evolved
	}
	return combined, nil
}

func splitIngestBatches(events []usage.Event) ([][]usage.Event, error) {
	envelopeBytes := len(`{"events":[]}`) - 2
	var batches [][]usage.Event
	var batch []usage.Event
	batchBytes := envelopeBytes
	for _, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			return nil, err
		}
		separatorBytes := 0
		if len(batch) > 0 {
			separatorBytes = 1
		}
		if len(batch) > 0 && batchBytes+separatorBytes+len(encoded) > maxIngestBatchBytes {
			batches = append(batches, batch)
			batch = nil
			batchBytes = envelopeBytes
			separatorBytes = 0
		}
		if batchBytes+separatorBytes+len(encoded) > maxIngestBatchBytes {
			return nil, fmt.Errorf("usage event exceeds the %d-byte ingest batch limit", maxIngestBatchBytes)
		}
		batch = append(batch, event)
		batchBytes += separatorBytes + len(encoded)
	}
	if len(batch) > 0 {
		batches = append(batches, batch)
	}
	return batches, nil
}

func (c Client) ingestBatch(ctx context.Context, events []usage.Event) (database.IngestResult, error) {
	var result database.IngestResult
	if err := c.postJSON(ctx, "/api/v1/events/batch", batchRequest{Events: events}, &result); err != nil {
		return database.IngestResult{}, err
	}
	return result, nil
}

func (c Client) Heartbeat(ctx context.Context, heartbeat HeartbeatRequest) error {
	return c.postJSON(ctx, "/api/v1/agents/heartbeat", heartbeat, nil)
}

// Health verifies that the hub is reachable before the agent performs an
// expensive local collection pass. Offline hubs therefore trigger the normal
// failure backoff without repeatedly parsing unchanged provider histories.
func (c Client) Health(ctx context.Context) error {
	if strings.TrimSpace(c.ServerURL) == "" {
		return fmt.Errorf("server URL is required")
	}
	requestContext, cancel := c.requestContext(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, strings.TrimRight(c.ServerURL, "/")+"/healthz", nil)
	if err != nil {
		return err
	}
	if c.Token != "" {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return fmt.Errorf("server returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func (c Client) postJSON(ctx context.Context, path string, payload any, result any) error {
	if strings.TrimSpace(c.ServerURL) == "" {
		return fmt.Errorf("server URL is required")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	requestContext, cancel := c.requestContext(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, strings.TrimRight(c.ServerURL, "/")+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return fmt.Errorf("server returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	if result == nil {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
