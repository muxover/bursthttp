package client

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// All methods must be safe for concurrent use.
type MetricsCollector interface {
	RecordRequest(method, host string) time.Time
	RecordResponse(method, host string, statusCode int, err error, start time.Time, bytesWritten, bytesRead int64)
	RecordPoolEvent(event PoolEvent, host string)
	Snapshot() MetricsSnapshot
}

type PoolEvent int

const (
	PoolEventConnCreated PoolEvent = iota
	PoolEventConnClosed
	PoolEventConnReused
	PoolEventConnFailed
)

type MetricsSnapshot struct {
	RequestsTotal int64
	RequestsOK    int64
	RequestsError int64
	Requests4xx   int64
	Requests5xx   int64
	Requests1xx   int64
	Requests3xx   int64
	RetriesTotal  int64

	BytesWritten int64
	BytesRead    int64

	LatencyP50 time.Duration
	LatencyP95 time.Duration
	LatencyP99 time.Duration
	LatencyAvg time.Duration
	LatencyMin time.Duration
	LatencyMax time.Duration

	ConnsCreated int64
	ConnsClosed  int64
	ConnsReused  int64
	ConnsFailed  int64
}

type BuiltinMetrics struct {
	requestsTotal atomic.Int64
	requestsOK    atomic.Int64
	requestsError atomic.Int64
	requests1xx   atomic.Int64
	requests3xx   atomic.Int64
	requests4xx   atomic.Int64
	requests5xx   atomic.Int64
	retriesTotal  atomic.Int64

	bytesWritten atomic.Int64
	bytesRead    atomic.Int64

	connsCreated atomic.Int64
	connsClosed  atomic.Int64
	connsReused  atomic.Int64
	connsFailed  atomic.Int64

	latencyMu  sync.Mutex
	latencies  []time.Duration
	maxSamples int
}

func NewBuiltinMetrics() *BuiltinMetrics {
	return &BuiltinMetrics{
		latencies:  make([]time.Duration, 0, 1024),
		maxSamples: 10000,
	}
}

func (m *BuiltinMetrics) RecordRequest(method, host string) time.Time {
	return time.Now()
}

func (m *BuiltinMetrics) RecordResponse(method, host string, statusCode int, err error, start time.Time, bytesWritten, bytesRead int64) {
	m.requestsTotal.Add(1)
	m.bytesWritten.Add(bytesWritten)
	m.bytesRead.Add(bytesRead)

	switch {
	case err != nil && statusCode == 0:
		m.requestsError.Add(1)
	case statusCode >= 100 && statusCode < 200:
		m.requests1xx.Add(1)
	case statusCode >= 200 && statusCode < 300:
		m.requestsOK.Add(1)
	case statusCode >= 300 && statusCode < 400:
		m.requests3xx.Add(1)
	case statusCode >= 400 && statusCode < 500:
		m.requests4xx.Add(1)
	case statusCode >= 500:
		m.requests5xx.Add(1)
	}

	if !start.IsZero() {
		latency := time.Since(start)
		m.latencyMu.Lock()
		if len(m.latencies) >= m.maxSamples {
			copy(m.latencies, m.latencies[len(m.latencies)/2:])
			m.latencies = m.latencies[:len(m.latencies)/2]
		}
		m.latencies = append(m.latencies, latency)
		m.latencyMu.Unlock()
	}
}

func (m *BuiltinMetrics) RecordPoolEvent(event PoolEvent, host string) {
	switch event {
	case PoolEventConnCreated:
		m.connsCreated.Add(1)
	case PoolEventConnClosed:
		m.connsClosed.Add(1)
	case PoolEventConnReused:
		m.connsReused.Add(1)
	case PoolEventConnFailed:
		m.connsFailed.Add(1)
	}
}

func (m *BuiltinMetrics) RecordRetry() {
	m.retriesTotal.Add(1)
}

func (m *BuiltinMetrics) Snapshot() MetricsSnapshot {
	snap := MetricsSnapshot{
		RequestsTotal: m.requestsTotal.Load(),
		RequestsOK:    m.requestsOK.Load(),
		RequestsError: m.requestsError.Load(),
		Requests1xx:   m.requests1xx.Load(),
		Requests3xx:   m.requests3xx.Load(),
		Requests4xx:   m.requests4xx.Load(),
		Requests5xx:   m.requests5xx.Load(),
		RetriesTotal:  m.retriesTotal.Load(),
		BytesWritten:  m.bytesWritten.Load(),
		BytesRead:     m.bytesRead.Load(),
		ConnsCreated:  m.connsCreated.Load(),
		ConnsClosed:   m.connsClosed.Load(),
		ConnsReused:   m.connsReused.Load(),
		ConnsFailed:   m.connsFailed.Load(),
	}

	m.latencyMu.Lock()
	n := len(m.latencies)
	if n > 0 {
		sorted := make([]time.Duration, n)
		copy(sorted, m.latencies)
		m.latencyMu.Unlock()

		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

		snap.LatencyMin = sorted[0]
		snap.LatencyMax = sorted[n-1]
		snap.LatencyP50 = sorted[n*50/100]
		snap.LatencyP95 = sorted[n*95/100]
		p99Idx := n * 99 / 100
		if p99Idx >= n {
			p99Idx = n - 1
		}
		snap.LatencyP99 = sorted[p99Idx]

		var total time.Duration
		for _, d := range sorted {
			total += d
		}
		snap.LatencyAvg = total / time.Duration(n)
	} else {
		m.latencyMu.Unlock()
	}

	return snap
}
