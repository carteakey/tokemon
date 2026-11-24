package database

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"

	"github.com/tokemon/tokemon/internal/usage"
)

const maxSessionDetailTimelineEvents = 500

// ErrSessionNotFound indicates that no stored metadata exists for a session ID.
var ErrSessionNotFound = sql.ErrNoRows

// SessionDetail holds strictly metadata-only aggregates, per-model breakdown,
// and bounded timeline events for one session ID. Prompts, responses, raw
// local paths, and conversation titles are never stored or returned.
type SessionDetail struct {
	SessionID         string               `json:"session_id"`
	Events            int64                `json:"events"`
	FirstSeen         string               `json:"first_seen"`
	LastSeen          string               `json:"last_seen"`
	WallDurationMS    int64                `json:"wall_duration_ms"`
	DurationMS        *int64               `json:"duration_ms,omitempty"`
	Machine           string               `json:"machine"`
	Machines          []string             `json:"machines"`
	Provider          string               `json:"provider"`
	Providers         []string             `json:"providers"`
	Tool              string               `json:"tool"`
	Tools             []string             `json:"tools"`
	Project           string               `json:"project,omitempty"`
	Projects          []string             `json:"projects,omitempty"`
	Models            []string             `json:"models"`
	ModelSwitches     int                  `json:"model_switches"`
	InputTokens       *int64               `json:"input_tokens,omitempty"`
	OutputTokens      *int64               `json:"output_tokens,omitempty"`
	CacheReadTokens   *int64               `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens  *int64               `json:"cache_write_tokens,omitempty"`
	ReasoningTokens   *int64               `json:"reasoning_tokens,omitempty"`
	TotalTokens       *int64               `json:"total_tokens,omitempty"`
	Cost              *float64             `json:"cost,omitempty"`
	CostEstimated     bool                 `json:"cost_estimated,omitempty"`
	Currency          string               `json:"currency,omitempty"`
	Cache             CacheSummary         `json:"cache"`
	Accuracy          usage.Accuracy       `json:"accuracy"`
	ModelBreakdown    []SessionDetailModel `json:"model_breakdown"`
	Timeline          []SessionDetailEvent `json:"timeline"`
	TimelineTruncated bool                 `json:"timeline_truncated,omitempty"`
}

type SessionDetailModel struct {
	Model            string         `json:"model"`
	Provider         string         `json:"provider"`
	Events           int64          `json:"events"`
	InputTokens      *int64         `json:"input_tokens,omitempty"`
	OutputTokens     *int64         `json:"output_tokens,omitempty"`
	CacheReadTokens  *int64         `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *int64         `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  *int64         `json:"reasoning_tokens,omitempty"`
	TotalTokens      *int64         `json:"total_tokens,omitempty"`
	Cost             *float64       `json:"cost,omitempty"`
	CostEstimated    bool           `json:"cost_estimated,omitempty"`
	Cache            CacheSummary   `json:"cache"`
	DurationMS       *int64         `json:"duration_ms,omitempty"`
	FirstSeen        string         `json:"first_seen"`
	LastSeen         string         `json:"last_seen"`
	Accuracy         usage.Accuracy `json:"accuracy"`
}

type SessionDetailEvent struct {
	EventID          string         `json:"event_id"`
	Timestamp        string         `json:"timestamp"`
	Machine          string         `json:"machine"`
	Provider         string         `json:"provider"`
	Model            string         `json:"model"`
	Tool             string         `json:"tool"`
	Project          string         `json:"project,omitempty"`
	InputTokens      *int64         `json:"input_tokens,omitempty"`
	OutputTokens     *int64         `json:"output_tokens,omitempty"`
	CacheReadTokens  *int64         `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *int64         `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  *int64         `json:"reasoning_tokens,omitempty"`
	TotalTokens      *int64         `json:"total_tokens,omitempty"`
	Cost             *float64       `json:"cost,omitempty"`
	CostEstimated    bool           `json:"cost_estimated,omitempty"`
	DurationMS       *int64         `json:"duration_ms,omitempty"`
	Accuracy         usage.Accuracy `json:"accuracy"`
}

type nullableIntSum struct {
	sum   int64
	valid bool
}

func newNullableIntSum() nullableIntSum {
	return nullableIntSum{valid: true}
}

func (s *nullableIntSum) add(val *int64) {
	if val == nil {
		s.valid = false
		return
	}
	if s.valid {
		s.sum += *val
	}
}

func (s nullableIntSum) ptr() *int64 {
	if !s.valid {
		return nil
	}
	v := s.sum
	return &v
}

// SessionDetail returns strictly metadata-only usage details for sessionID, or
// ErrSessionNotFound (sql.ErrNoRows) when missing/empty.
func (s *Store) SessionDetail(ctx context.Context, sessionID string) (SessionDetail, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return SessionDetail{}, ErrSessionNotFound
	}

	rows, err := s.readHandle().QueryContext(ctx, `SELECT
event_id, timestamp, machine_id, project, provider, raw_model, canonical_model, tool,
input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens,
total_tokens, cost, cost_estimated, currency, duration_ms, token_accuracy
FROM usage_events WHERE session_id = ?
ORDER BY timestamp ASC, event_id ASC`, sessionID)
	if err != nil {
		return SessionDetail{}, err
	}
	defer rows.Close()

	type parsedEvent struct {
		event    SessionDetailEvent
		parsedAt time.Time
		hasTime  bool
		currency string
	}

	var rawEvents []parsedEvent
	for rows.Next() {
		var item SessionDetailEvent
		var rawModel, canonicalModel, currencyStr, accuracyStr string
		var input, output, cacheRead, cacheWrite, reasoning, total, duration sql.NullInt64
		var cost sql.NullFloat64
		var costEst int64
		if err := rows.Scan(
			&item.EventID, &item.Timestamp, &item.Machine, &item.Project, &item.Provider,
			&rawModel, &canonicalModel, &item.Tool,
			&input, &output, &cacheRead, &cacheWrite, &reasoning, &total,
			&cost, &costEst, &currencyStr, &duration, &accuracyStr,
		); err != nil {
			return SessionDetail{}, err
		}
		item.Model = canonicalModel
		if item.Model == "" {
			item.Model = rawModel
		}
		if item.Model == "" {
			item.Model = "Unknown"
		}
		item.InputTokens = nullInt64(input)
		item.OutputTokens = nullInt64(output)
		item.CacheReadTokens = nullInt64(cacheRead)
		item.CacheWriteTokens = nullInt64(cacheWrite)
		item.ReasoningTokens = nullInt64(reasoning)
		item.TotalTokens = nullInt64(total)
		item.Cost = nullFloat64(cost)
		item.CostEstimated = costEst != 0
		item.DurationMS = nullInt64(duration)
		item.Accuracy = usage.Accuracy(accuracyStr)
		if item.Accuracy == "" {
			item.Accuracy = usage.AccuracyUnknown
		}
		parsedAt, parseErr := timeParse(item.Timestamp)
		rawEvents = append(rawEvents, parsedEvent{
			event:    item,
			parsedAt: parsedAt,
			hasTime:  parseErr == nil,
			currency: currencyStr,
		})
	}
	if err := rows.Err(); err != nil {
		return SessionDetail{}, err
	}
	if len(rawEvents) == 0 {
		return SessionDetail{}, ErrSessionNotFound
	}

	sort.SliceStable(rawEvents, func(left, right int) bool {
		if rawEvents[left].hasTime && rawEvents[right].hasTime {
			if !rawEvents[left].parsedAt.Equal(rawEvents[right].parsedAt) {
				return rawEvents[left].parsedAt.Before(rawEvents[right].parsedAt)
			}
		} else if rawEvents[left].event.Timestamp != rawEvents[right].event.Timestamp {
			return rawEvents[left].event.Timestamp < rawEvents[right].event.Timestamp
		}
		return rawEvents[left].event.EventID < rawEvents[right].event.EventID
	})

	detail := SessionDetail{
		SessionID: sessionID,
		Events:    int64(len(rawEvents)),
		FirstSeen: rawEvents[0].event.Timestamp,
		LastSeen:  rawEvents[len(rawEvents)-1].event.Timestamp,
	}

	if rawEvents[0].hasTime && rawEvents[len(rawEvents)-1].hasTime {
		wall := rawEvents[len(rawEvents)-1].parsedAt.Sub(rawEvents[0].parsedAt).Milliseconds()
		if wall > 0 {
			detail.WallDurationMS = wall
		}
	}

	inSum := newNullableIntSum()
	outSum := newNullableIntSum()
	crSum := newNullableIntSum()
	cwSum := newNullableIntSum()
	reasonSum := newNullableIntSum()
	totSum := newNullableIntSum()
	durSum := newNullableIntSum()
	var (
		hasCost       bool
		costSum       float64
		accuracy      usage.Accuracy
		accuracyMixed bool
		seenMachines  = make(map[string]struct{})
		seenProviders = make(map[string]struct{})
		seenTools     = make(map[string]struct{})
		seenProjects  = make(map[string]struct{})
		seenModels    = make(map[string]struct{})
		prevModelPair string
		hasPrevModel  bool
	)

	type modelAccum struct {
		provider      string
		model         string
		events        int64
		inSum         nullableIntSum
		outSum        nullableIntSum
		crSum         nullableIntSum
		cwSum         nullableIntSum
		reasonSum     nullableIntSum
		totSum        nullableIntSum
		durSum        nullableIntSum
		hasCost       bool
		costSum       float64
		costEstimated bool
		cache         CacheSummary
		firstSeen     string
		lastSeen      string
		accuracy      usage.Accuracy
		accuracyMixed bool
	}
	modelOrder := make([]string, 0, 4)
	modelMap := make(map[string]*modelAccum)

	for _, item := range rawEvents {
		ev := item.event
		if ev.Machine != "" {
			if _, ok := seenMachines[ev.Machine]; !ok {
				seenMachines[ev.Machine] = struct{}{}
				detail.Machines = append(detail.Machines, ev.Machine)
			}
			if detail.Machine == "" {
				detail.Machine = ev.Machine
			}
		}
		if ev.Provider != "" {
			if _, ok := seenProviders[ev.Provider]; !ok {
				seenProviders[ev.Provider] = struct{}{}
				detail.Providers = append(detail.Providers, ev.Provider)
			}
			if detail.Provider == "" {
				detail.Provider = ev.Provider
			}
		}
		if ev.Tool != "" {
			if _, ok := seenTools[ev.Tool]; !ok {
				seenTools[ev.Tool] = struct{}{}
				detail.Tools = append(detail.Tools, ev.Tool)
			}
			if detail.Tool == "" {
				detail.Tool = ev.Tool
			}
		}
		if ev.Project != "" {
			if _, ok := seenProjects[ev.Project]; !ok {
				seenProjects[ev.Project] = struct{}{}
				detail.Projects = append(detail.Projects, ev.Project)
			}
			if detail.Project == "" {
				detail.Project = ev.Project
			}
		}
		if _, ok := seenModels[ev.Model]; !ok {
			seenModels[ev.Model] = struct{}{}
			detail.Models = append(detail.Models, ev.Model)
		}
		pair := ev.Provider + "\x1f" + ev.Model
		if hasPrevModel && pair != prevModelPair {
			detail.ModelSwitches++
		}
		prevModelPair = pair
		hasPrevModel = true

		if detail.Currency == "" && item.currency != "" {
			detail.Currency = item.currency
		}
		if !accuracyMixed {
			if accuracy == "" {
				accuracy = ev.Accuracy
			} else if accuracy != ev.Accuracy {
				accuracyMixed = true
			}
		}

		inSum.add(ev.InputTokens)
		outSum.add(ev.OutputTokens)
		crSum.add(ev.CacheReadTokens)
		cwSum.add(ev.CacheWriteTokens)
		reasonSum.add(ev.ReasoningTokens)
		totSum.add(ev.TotalTokens)
		durSum.add(ev.DurationMS)
		if ev.Cost != nil {
			hasCost = true
			costSum += *ev.Cost
		}
		if ev.CostEstimated {
			detail.CostEstimated = true
		}
		if ev.InputTokens != nil && ev.CacheReadTokens != nil {
			detail.Cache.CachedTokens += *ev.CacheReadTokens
			detail.Cache.EligibleTokens += *ev.InputTokens + *ev.CacheReadTokens
		}

		acc := modelMap[pair]
		if acc == nil {
			acc = &modelAccum{
				provider:  ev.Provider,
				model:     ev.Model,
				inSum:     newNullableIntSum(),
				outSum:    newNullableIntSum(),
				crSum:     newNullableIntSum(),
				cwSum:     newNullableIntSum(),
				reasonSum: newNullableIntSum(),
				totSum:    newNullableIntSum(),
				durSum:    newNullableIntSum(),
				firstSeen: ev.Timestamp,
				accuracy:  ev.Accuracy,
			}
			modelMap[pair] = acc
			modelOrder = append(modelOrder, pair)
		}
		acc.events++
		acc.lastSeen = ev.Timestamp
		acc.inSum.add(ev.InputTokens)
		acc.outSum.add(ev.OutputTokens)
		acc.crSum.add(ev.CacheReadTokens)
		acc.cwSum.add(ev.CacheWriteTokens)
		acc.reasonSum.add(ev.ReasoningTokens)
		acc.totSum.add(ev.TotalTokens)
		acc.durSum.add(ev.DurationMS)
		if ev.Cost != nil {
			acc.hasCost = true
			acc.costSum += *ev.Cost
		}
		if ev.CostEstimated {
			acc.costEstimated = true
		}
		if ev.InputTokens != nil && ev.CacheReadTokens != nil {
			acc.cache.CachedTokens += *ev.CacheReadTokens
			acc.cache.EligibleTokens += *ev.InputTokens + *ev.CacheReadTokens
		}
		if acc.accuracy != ev.Accuracy {
			acc.accuracyMixed = true
		}
	}

	detail.InputTokens = inSum.ptr()
	detail.OutputTokens = outSum.ptr()
	detail.CacheReadTokens = crSum.ptr()
	detail.CacheWriteTokens = cwSum.ptr()
	detail.ReasoningTokens = reasonSum.ptr()
	detail.TotalTokens = totSum.ptr()
	detail.DurationMS = durSum.ptr()
	if hasCost {
		c := costSum
		detail.Cost = &c
	}
	if detail.Cache.EligibleTokens > 0 {
		detail.Cache.HitRate = float64(detail.Cache.CachedTokens) / float64(detail.Cache.EligibleTokens)
	}
	if accuracyMixed || accuracy == "" {
		detail.Accuracy = usage.AccuracyUnknown
	} else {
		detail.Accuracy = accuracy
	}

	detail.ModelBreakdown = make([]SessionDetailModel, 0, len(modelOrder))
	for _, key := range modelOrder {
		acc := modelMap[key]
		row := SessionDetailModel{
			Model:            acc.model,
			Provider:         acc.provider,
			Events:           acc.events,
			InputTokens:      acc.inSum.ptr(),
			OutputTokens:     acc.outSum.ptr(),
			CacheReadTokens:  acc.crSum.ptr(),
			CacheWriteTokens: acc.cwSum.ptr(),
			ReasoningTokens:  acc.reasonSum.ptr(),
			TotalTokens:      acc.totSum.ptr(),
			CostEstimated:    acc.costEstimated,
			Cache:            acc.cache,
			DurationMS:       acc.durSum.ptr(),
			FirstSeen:        acc.firstSeen,
			LastSeen:         acc.lastSeen,
			Accuracy:         acc.accuracy,
		}
		if acc.hasCost {
			c := acc.costSum
			row.Cost = &c
		}
		if row.Cache.EligibleTokens > 0 {
			row.Cache.HitRate = float64(row.Cache.CachedTokens) / float64(row.Cache.EligibleTokens)
		}
		if acc.accuracyMixed || row.Accuracy == "" {
			row.Accuracy = usage.AccuracyUnknown
		}
		detail.ModelBreakdown = append(detail.ModelBreakdown, row)
	}
	sort.SliceStable(detail.ModelBreakdown, func(left, right int) bool {
		lTok := detail.ModelBreakdown[left].TotalTokens
		rTok := detail.ModelBreakdown[right].TotalTokens
		switch {
		case lTok != nil && rTok != nil && *lTok != *rTok:
			return *lTok > *rTok
		case (lTok != nil) != (rTok != nil):
			return lTok != nil
		case detail.ModelBreakdown[left].Events != detail.ModelBreakdown[right].Events:
			return detail.ModelBreakdown[left].Events > detail.ModelBreakdown[right].Events
		default:
			return detail.ModelBreakdown[left].Model < detail.ModelBreakdown[right].Model
		}
	})

	startIdx := 0
	if len(rawEvents) > maxSessionDetailTimelineEvents {
		startIdx = len(rawEvents) - maxSessionDetailTimelineEvents
		detail.TimelineTruncated = true
	}
	detail.Timeline = make([]SessionDetailEvent, 0, len(rawEvents)-startIdx)
	for _, item := range rawEvents[startIdx:] {
		detail.Timeline = append(detail.Timeline, item.event)
	}

	return detail, nil
}
