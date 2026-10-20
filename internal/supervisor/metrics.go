package supervisor

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// metrics are the supervisor's Prometheus series, on a registry of its own.
type metrics struct {
	reg *prometheus.Registry

	requests  *prometheus.CounterVec   // kind, track, result
	duration  *prometheus.HistogramVec // kind, track, result
	queueWait *prometheus.HistogramVec // lane, priority
	jobTime   *prometheus.HistogramVec // lane, track: the time in the worker process
	attempts  *prometheus.CounterVec   // lane, outcome
	restarts  *prometheus.CounterVec   // lane, reason
	coalesced prometheus.Counter
	hedges    *prometheus.CounterVec // outcome
	inflight  *prometheus.GaugeVec   // lane
	queued    *prometheus.GaugeVec   // lane
	slots     *prometheus.GaugeVec   // lane, state
	slotRSS   *prometheus.GaugeVec   // slot
	slotInfo  *prometheus.GaugeVec   // slot, lane, backend, ffmpeg
	circuits  prometheus.Gauge
	breaker   *prometheus.CounterVec // to
}

var latencyBuckets = []float64{0.1, 0.25, 0.5, 0.75, 1, 1.5, 2, 3, 5, 8, 13, 20, 30, 60, 120}

func newMetrics() *metrics {
	m := &metrics{reg: prometheus.NewRegistry()}
	m.requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "live_worker_requests_total",
		Help: "Requests answered, by kind (segment, init, subtitle), track and result (ok or the error class).",
	}, []string{"kind", "track", "result"})
	m.duration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "live_worker_request_duration_seconds",
		Help:    "Time to answer a request, queueing and retries included.",
		Buckets: latencyBuckets,
	}, []string{"kind", "track", "result"})
	m.queueWait = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "live_worker_queue_wait_seconds",
		Help:    "Time a job waited for a slot.",
		Buckets: []float64{0.001, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30},
	}, []string{"lane", "priority"})
	m.jobTime = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "live_worker_job_seconds",
		Help:    "Time a successful job took in its worker process.",
		Buckets: latencyBuckets,
	}, []string{"lane", "track"})
	m.attempts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "live_worker_attempts_total",
		Help: "Attempts on worker processes, by outcome (ok, source, busy, deadline, internal, crash, timeout, bad-request).",
	}, []string{"lane", "outcome"})
	m.restarts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "live_worker_slot_restarts_total",
		Help: "Worker processes that ended and were started again, by reason (exit, rss, wedged, drain, broken).",
	}, []string{"lane", "reason"})
	m.coalesced = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "live_worker_coalesced_total",
		Help: "Requests served by an identical job already in flight.",
	})
	m.hedges = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "live_worker_hedges_total",
		Help: "Second attempts started on an idle slot for a slow playhead job, by outcome (started, won).",
	}, []string{"outcome"})
	m.inflight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "live_worker_inflight",
		Help: "Jobs running in worker processes.",
	}, []string{"lane"})
	m.queued = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "live_worker_queued",
		Help: "Playhead jobs waiting for a slot.",
	}, []string{"lane"})
	m.slots = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "live_worker_slots",
		Help: "Worker processes by state (idle, busy, draining, starting, down).",
	}, []string{"lane", "state"})
	m.slotRSS = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "live_worker_slot_rss_bytes",
		Help: "A worker process's resident set.",
	}, []string{"slot"})
	m.slotInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "live_worker_slot_info",
		Help: "A worker process's backend (cuda, cpu) and FFmpeg version, 1 while it is up.",
	}, []string{"slot", "lane", "backend", "ffmpeg"})
	m.circuits = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "live_worker_circuits_open",
		Help: "Sources whose circuit is open or half-open.",
	})
	m.breaker = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "live_worker_circuit_transitions_total",
		Help: "Circuit state changes, by the state entered (open, half-open, closed).",
	}, []string{"to"})
	m.reg.MustRegister(m.requests, m.duration, m.queueWait, m.jobTime, m.attempts, m.restarts, m.coalesced, m.hedges,
		m.inflight, m.queued, m.slots, m.slotRSS, m.slotInfo, m.circuits, m.breaker,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}
