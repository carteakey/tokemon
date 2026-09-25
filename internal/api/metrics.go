package api

import (
	"sync/atomic"
	"time"
)

// Metrics contains process-local operational counters. Values are monotonic
// for the life of the server; stale-agent count is refreshed from SQLite when
// the metrics endpoint is read.
type Metrics struct {
	requestsTotal     atomic.Uint64
	requestFailures   atomic.Uint64
	ingestRequests    atomic.Uint64
	ingestAccepted    atomic.Uint64
	ingestDuplicates  atomic.Uint64
	ingestRejected    atomic.Uint64
	sourceErrors      atomic.Uint64
	dbOperations      atomic.Uint64
	dbLatencyNanos    atomic.Uint64
	dbLatencyMaxNanos atomic.Uint64
}

// MetricsSnapshot is the stable JSON shape returned by GET /metrics.
type MetricsSnapshot struct {
	RequestsTotal        uint64 `json:"requests_total"`
	RequestFailures      uint64 `json:"request_failures"`
	IngestRequests       uint64 `json:"ingest_requests"`
	IngestAcceptedEvents uint64 `json:"ingest_accepted_events"`
	IngestDuplicates     uint64 `json:"ingest_duplicates"`
	IngestRejectedEvents uint64 `json:"ingest_rejected_events"`
	SourceErrors         uint64 `json:"source_errors"`
	StaleAgents          int    `json:"stale_agents"`
	DBOperations         uint64 `json:"db_operations"`
	DBLatencyTotalMS     uint64 `json:"db_latency_total_ms"`
	DBLatencyMaxMS       uint64 `json:"db_latency_max_ms"`
}

func NewMetrics() *Metrics { return &Metrics{} }

func (m *Metrics) snapshot() MetricsSnapshot {
	totalNanos := m.dbLatencyNanos.Load()
	maxNanos := m.dbLatencyMaxNanos.Load()
	return MetricsSnapshot{
		RequestsTotal:        m.requestsTotal.Load(),
		RequestFailures:      m.requestFailures.Load(),
		IngestRequests:       m.ingestRequests.Load(),
		IngestAcceptedEvents: m.ingestAccepted.Load(),
		IngestDuplicates:     m.ingestDuplicates.Load(),
		IngestRejectedEvents: m.ingestRejected.Load(),
		SourceErrors:         m.sourceErrors.Load(),
		DBOperations:         m.dbOperations.Load(),
		DBLatencyTotalMS:     latencyMilliseconds(totalNanos),
		DBLatencyMaxMS:       latencyMilliseconds(maxNanos),
	}
}

func latencyMilliseconds(nanos uint64) uint64 {
	if nanos == 0 {
		return 0
	}
	milliseconds := uint64(time.Duration(nanos) / time.Millisecond)
	if milliseconds == 0 {
		return 1
	}
	return milliseconds
}

func (m *Metrics) observeDB(start time.Time) {
	latency := time.Since(start)
	nanos := uint64(latency)
	m.dbOperations.Add(1)
	m.dbLatencyNanos.Add(nanos)
	for {
		current := m.dbLatencyMaxNanos.Load()
		if current >= nanos || m.dbLatencyMaxNanos.CompareAndSwap(current, nanos) {
			return
		}
	}
}
