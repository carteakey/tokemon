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

	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/usage"
)

type Client struct {
	ServerURL  string
	Token      string
	HTTPClient *http.Client
}

type batchRequest struct {
	Events []usage.Event `json:"events"`
}

func (c Client) Ingest(ctx context.Context, events []usage.Event) (database.IngestResult, error) {
	if strings.TrimSpace(c.ServerURL) == "" {
		return database.IngestResult{}, fmt.Errorf("server URL is required")
	}
	if len(events) == 0 {
		return database.IngestResult{}, fmt.Errorf("events must not be empty")
	}
	payload, err := json.Marshal(batchRequest{Events: events})
	if err != nil {
		return database.IngestResult{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.ServerURL, "/")+"/api/v1/events/batch", bytes.NewReader(payload))
	if err != nil {
		return database.IngestResult{}, err
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
		return database.IngestResult{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return database.IngestResult{}, fmt.Errorf("server returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var result database.IngestResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return database.IngestResult{}, fmt.Errorf("decode ingest response: %w", err)
	}
	return result, nil
}
