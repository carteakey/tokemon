package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/quota"
)

const (
	alertHTTPTimeout    = 5 * time.Second
	alertSWRRecheckWait = 2100 * time.Millisecond
	alertSWRSettleWait  = 150 * time.Millisecond
)

type quotaAlertKey struct {
	QuotaKey    string
	PeriodStart string
	BudgetSig   string
	Threshold   string
}

type quotaAlertPayload struct {
	SchemaVersion string   `json:"schema_version"`
	Event         string   `json:"event"`
	Threshold     string   `json:"threshold"`
	State         string   `json:"state"`
	Label         string   `json:"label"`
	Provider      string   `json:"provider,omitempty"`
	Model         string   `json:"model,omitempty"`
	PeriodStart   string   `json:"period_start"`
	PeriodEnd     string   `json:"period_end"`
	Percent       float64  `json:"percent"`
	TokensUsed    int64    `json:"tokens_used"`
	TokensBudget  *int64   `json:"tokens_budget,omitempty"`
	CostUsed      *float64 `json:"cost_used,omitempty"`
	CostBudget    *float64 `json:"cost_budget,omitempty"`
	TriggeredAt   string   `json:"triggered_at"`
}

type quotaAlertState struct {
	evalMu         sync.Mutex
	mu             sync.Mutex
	sent           map[quotaAlertKey]struct{}
	recheckRunning bool
	recheckNeeded  bool
	recheckGen     uint64
	recheckWait    time.Duration
	recheckSettle  time.Duration
	client         *http.Client
}

func newQuotaAlertState(customClient *http.Client) *quotaAlertState {
	client := customClient
	if client == nil {
		client = &http.Client{Timeout: alertHTTPTimeout}
	}
	return &quotaAlertState{
		sent:          make(map[quotaAlertKey]struct{}),
		recheckWait:   alertSWRRecheckWait,
		recheckSettle: alertSWRSettleWait,
		client:        client,
	}
}

func (s *Server) alertingEnabled() bool {
	return s.config.AlertWebhookURL != "" || s.config.AlertNtfyURL != ""
}

func (s *Server) resetAlertStateForQuota(provider, model string) {
	if s.alerts == nil {
		return
	}
	targetKey := quota.Key(provider, model)
	s.alerts.mu.Lock()
	for k := range s.alerts.sent {
		if k.QuotaKey == targetKey {
			delete(s.alerts.sent, k)
		}
	}
	s.alerts.mu.Unlock()
}

func (s *Server) triggerQuotaAlerts(fromIngest bool) {
	if !s.alertingEnabled() || s.alerts == nil {
		return
	}
	if !s.tryStartBackground() {
		return
	}
	go func() {
		defer s.bgWG.Done()
		ctx, cancel := context.WithTimeout(s.backgroundContext(), 15*time.Second)
		s.evaluateAndSendQuotaAlerts(ctx)
		cancel()

		if !fromIngest {
			return
		}
		// Store.QuotaStatus uses a 2s SWR coalescing window on ingest and returns
		// the stale cached bucket while refreshing asynchronously; run a
		// coalesced follow-up after the 2s window that primes the SWR refresh
		// and then evaluates the freshly published bucket.
		s.alerts.mu.Lock()
		s.alerts.recheckNeeded = true
		if s.alerts.recheckRunning {
			s.alerts.mu.Unlock()
			return
		}
		s.alerts.recheckRunning = true
		s.alerts.recheckGen++
		myGen := s.alerts.recheckGen
		s.alerts.mu.Unlock()

		defer func() {
			s.alerts.mu.Lock()
			if s.alerts.recheckGen == myGen {
				s.alerts.recheckRunning = false
			}
			s.alerts.mu.Unlock()
		}()

		for {
			s.alerts.mu.Lock()
			if !s.alerts.recheckNeeded {
				s.alerts.recheckRunning = false
				s.alerts.mu.Unlock()
				return
			}
			s.alerts.recheckNeeded = false
			recheckWait := s.alerts.recheckWait
			recheckSettle := s.alerts.recheckSettle
			s.alerts.mu.Unlock()
			if recheckWait <= 0 {
				recheckWait = alertSWRRecheckWait
			}
			if recheckSettle <= 0 {
				recheckSettle = alertSWRSettleWait
			}

			timer := time.NewTimer(recheckWait)
			select {
			case <-timer.C:
			case <-s.backgroundContext().Done():
				timer.Stop()
				return
			}

			recheckCtx, recheckCancel := context.WithTimeout(s.backgroundContext(), 15*time.Second)
			_ = s.store.QuotaStatus(recheckCtx, time.Now())
			settle := time.NewTimer(recheckSettle)
			select {
			case <-settle.C:
			case <-s.backgroundContext().Done():
				settle.Stop()
				recheckCancel()
				return
			}
			s.evaluateAndSendQuotaAlerts(recheckCtx)
			recheckCancel()
		}
	}()
}

func (s *Server) evaluateAndSendQuotaAlerts(ctx context.Context) {
	if !s.alertingEnabled() || s.alerts == nil || ctx.Err() != nil {
		return
	}
	s.alerts.evalMu.Lock()
	defer s.alerts.evalMu.Unlock()

	now := time.Now()
	statuses := s.store.QuotaStatus(ctx, now)
	if len(statuses) == 0 {
		return
	}
	for _, status := range statuses {
		if ctx.Err() != nil {
			return
		}
		pct, threshold := quotaAlertThreshold(status)
		qKey := quota.Key(status.Provider, status.Model)
		bSig := quotaBudgetSignature(status)

		s.alerts.mu.Lock()
		// Prune stale periods or superseded budgets for this quota key.
		for k := range s.alerts.sent {
			if k.QuotaKey == qKey && (k.PeriodStart != status.PeriodStart || k.BudgetSig != bSig) {
				delete(s.alerts.sent, k)
			}
		}
		if threshold == "" {
			s.alerts.mu.Unlock()
			continue
		}
		alertKey := quotaAlertKey{
			QuotaKey:    qKey,
			PeriodStart: status.PeriodStart,
			BudgetSig:   bSig,
			Threshold:   threshold,
		}
		if _, alreadySent := s.alerts.sent[alertKey]; alreadySent {
			s.alerts.mu.Unlock()
			continue
		}
		s.alerts.mu.Unlock()

		payload := quotaAlertPayload{
			SchemaVersion: "1",
			Event:         "quota_threshold",
			Threshold:     threshold,
			State:         status.State,
			Label:         quotaDisplayLabel(status),
			Provider:      status.Provider,
			Model:         status.Model,
			PeriodStart:   status.PeriodStart,
			PeriodEnd:     status.PeriodEnd,
			Percent:       math.Round(pct*10) / 10,
			TokensUsed:    status.TokensUsed,
			TokensBudget:  status.TokensBudget,
			CostUsed:      status.CostUsed,
			CostBudget:    status.CostBudget,
			TriggeredAt:   now.UTC().Format(time.RFC3339),
		}
		if s.dispatchQuotaAlert(ctx, payload) {
			s.alerts.mu.Lock()
			s.alerts.sent[alertKey] = struct{}{}
			s.alerts.mu.Unlock()
		}
	}
}

func quotaBudgetSignature(status database.QuotaStatus) string {
	var tok, cost string
	if status.TokensBudget != nil {
		tok = fmt.Sprintf("%d", *status.TokensBudget)
	}
	if status.CostBudget != nil {
		cost = fmt.Sprintf("%.6f", *status.CostBudget)
	}
	return tok + "|" + cost
}

func quotaAlertThreshold(status database.QuotaStatus) (float64, string) {
	if status.State == "unknown" {
		return 0, ""
	}
	var maxPct float64
	var measured bool
	if status.TokensBudget != nil && *status.TokensBudget > 0 {
		maxPct = float64(status.TokensUsed) * 100 / float64(*status.TokensBudget)
		measured = true
	}
	if status.CostBudget != nil && *status.CostBudget > 0 && status.CostUsed != nil {
		costPct := *status.CostUsed * 100 / *status.CostBudget
		if !measured || costPct > maxPct {
			maxPct = costPct
		}
		measured = true
	}
	if !measured {
		return 0, ""
	}
	if status.State == "over" || maxPct >= 100 {
		return maxPct, "overage"
	}
	if maxPct >= 80 {
		return maxPct, "warning"
	}
	return maxPct, ""
}

func (s *Server) dispatchQuotaAlert(ctx context.Context, payload quotaAlertPayload) bool {
	delivered := false
	if s.config.AlertWebhookURL != "" {
		if err := s.postQuotaWebhook(ctx, s.config.AlertWebhookURL, payload); err != nil {
			s.logger.Warn("quota_alert_webhook_failed", "threshold", payload.Threshold, "error", err.Error())
		} else {
			delivered = true
		}
	}
	if s.config.AlertNtfyURL != "" {
		if err := s.postQuotaNtfy(ctx, s.config.AlertNtfyURL, payload); err != nil {
			s.logger.Warn("quota_alert_ntfy_failed", "threshold", payload.Threshold, "error", err.Error())
		} else {
			delivered = true
		}
	}
	return delivered
}

func (s *Server) postQuotaWebhook(ctx context.Context, targetURL string, payload quotaAlertPayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	reqCtx, cancel := context.WithTimeout(ctx, alertHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := s.alerts.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook status %d", resp.StatusCode)
	}
	return nil
}

func (s *Server) postQuotaNtfy(ctx context.Context, targetURL string, payload quotaAlertPayload) error {
	title := sanitizeAlertHeader(fmt.Sprintf("Tokemon quota %s: %s", payload.Threshold, payload.Label))
	priority := "default"
	tags := "warning,tokemon"
	if payload.Threshold == "overage" {
		priority = "high"
		tags = "rotating_light,tokemon"
	}
	var usageSummary string
	switch {
	case payload.TokensBudget != nil:
		usageSummary = fmt.Sprintf("%s / %s tokens", commas(payload.TokensUsed), commas(*payload.TokensBudget))
	case payload.CostBudget != nil && payload.CostUsed != nil:
		usageSummary = fmt.Sprintf("%s / %s", money(*payload.CostUsed), money(*payload.CostBudget))
	}
	message := fmt.Sprintf("%s reached %.1f%% (%s) for period %s to %s.",
		payload.Label, payload.Percent, usageSummary, payload.PeriodStart, payload.PeriodEnd)

	reqCtx, cancel := context.WithTimeout(ctx, alertHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, targetURL, strings.NewReader(message))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("Title", title)
	req.Header.Set("Priority", priority)
	req.Header.Set("Tags", tags)
	resp, err := s.alerts.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy status %d", resp.StatusCode)
	}
	return nil
}

func sanitizeAlertHeader(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		if r == '\r' || r == '\n' || r < 0x20 || r == 0x7f {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}
