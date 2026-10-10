package sched

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics owns the Prometheus collectors for one scheduler. Labels avoid
// combining principal, backend, and model on one metric to bound cardinality.
type Metrics struct {
	registry           *prometheus.Registry
	dispatchTotal      *prometheus.CounterVec
	slotInFlight       *prometheus.GaugeVec
	admissionInFlight  *prometheus.GaugeVec
	swapEventTotal     *prometheus.CounterVec
	vtcMaxMinRatio     prometheus.Gauge
	resolverStageTotal *prometheus.CounterVec
	resolvedModelTotal *prometheus.CounterVec
	retryFreeIssued    prometheus.Counter
	jobsActive         prometheus.Gauge
}

// NewMetrics constructs and registers an isolated collector set. A nil
// registry creates a new registry owned by the returned Metrics.
func NewMetrics(registry *prometheus.Registry) *Metrics {
	if registry == nil {
		registry = prometheus.NewRegistry()
	}
	m := &Metrics{
		registry: registry,
		dispatchTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llm_dispatch_total",
			Help: "Total LLM dispatch outcomes. outcome is one of: dispatched, rate_limited, quota_exhausted, overloaded, slot_saturated, failover.",
		}, []string{"backend", "model", "outcome"}),
		slotInFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "llm_slot_in_flight",
			Help: "Scheduler-side per-(backend, model) in-flight dispatch slots.",
		}, []string{"backend", "model"}),
		admissionInFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "llm_admission_in_flight",
			Help: "Per-principal live admission count.",
		}, []string{"principal"}),
		swapEventTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llm_swap_event_total",
			Help: "Per-backend model swap events.",
		}, []string{"backend", "reason"}),
		vtcMaxMinRatio: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "llm_vtc_max_min_ratio",
			Help: "Ratio of maximum to minimum VTC units across live principals.",
		}),
		resolverStageTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llm_resolver_stage_total",
			Help: "Resolver outcomes by stage and outcome.",
		}, []string{"stage", "outcome"}),
		resolvedModelTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llm_resolved_model_total",
			Help: "Distribution of requested model names to resolved models.",
		}, []string{"requested", "resolved"}),
		retryFreeIssued: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "llm_retry_free_nonces_issued_total",
			Help: "Retry-free nonces issued on overload or slot saturation.",
		}),
		jobsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "llm_jobs_active",
			Help: "Live jobs in the scheduler job table.",
		}),
	}
	registry.MustRegister(
		m.dispatchTotal,
		m.slotInFlight,
		m.admissionInFlight,
		m.swapEventTotal,
		m.vtcMaxMinRatio,
		m.resolverStageTotal,
		m.resolvedModelTotal,
		m.retryFreeIssued,
		m.jobsActive,
	)
	return m
}

// Handler returns the Prometheus HTTP handler for this collector set.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// ObserveDispatch records one dispatch outcome.
func (m *Metrics) ObserveDispatch(backend, model, outcome string) {
	m.dispatchTotal.WithLabelValues(backend, model, outcome).Inc()
}

// ObserveSlotInFlight sets the current per-(backend, model) slot count.
func (m *Metrics) ObserveSlotInFlight(backend, model string, value int) {
	m.slotInFlight.WithLabelValues(backend, model).Set(float64(value))
}

// ObserveAdmissionInFlight sets a principal's current admission count.
func (m *Metrics) ObserveAdmissionInFlight(principalID string, value int) {
	if principalID == "" {
		return
	}
	m.admissionInFlight.WithLabelValues(principalID).Set(float64(value))
}

// ObserveSwapEvent counts a backend model-swap event.
func (m *Metrics) ObserveSwapEvent(backend, reason string) {
	m.swapEventTotal.WithLabelValues(backend, reason).Inc()
}

// ObserveResolverStage records the outcome of a resolver stage.
func (m *Metrics) ObserveResolverStage(stage, outcome string) {
	m.resolverStageTotal.WithLabelValues(stage, outcome).Inc()
}

// ObserveResolvedModel records a requested-to-resolved model mapping.
func (m *Metrics) ObserveResolvedModel(requested, resolved string) {
	if requested == resolved {
		return
	}
	m.resolvedModelTotal.WithLabelValues(requested, resolved).Inc()
}

// ObserveRetryFreeIssued increments the nonce issuance counter.
func (m *Metrics) ObserveRetryFreeIssued() {
	m.retryFreeIssued.Inc()
}

// ObserveVTCMaxMinRatio updates the fairness gauge from live VTC entries.
// It skips fewer than two principals; a zero minimum uses max as the value.
func (m *Metrics) ObserveVTCMaxMinRatio(a *Admitter) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.vtc) < 2 {
		return
	}
	var maxV, minV int64
	first := true
	for _, e := range a.vtc {
		if first {
			maxV, minV, first = e.Tokens, e.Tokens, false
			continue
		}
		if e.Tokens > maxV {
			maxV = e.Tokens
		}
		if e.Tokens < minV {
			minV = e.Tokens
		}
	}
	if minV <= 0 {
		m.vtcMaxMinRatio.Set(float64(maxV))
		return
	}
	m.vtcMaxMinRatio.Set(float64(maxV) / float64(minV))
}

// ObserveJobsActive updates the active-jobs gauge from a table.
func (m *Metrics) ObserveJobsActive(jt *JobTable) {
	jt.mu.Lock()
	n := len(jt.jobs)
	jt.mu.Unlock()
	m.jobsActive.Set(float64(n))
}
