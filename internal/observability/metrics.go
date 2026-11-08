package observability

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Recorder interface {
	SetRunStatus(status string, count int)
	ObserveTenantInflight(count int)
	IncStepRetry()
	IncModelFallback()
	ObserveModelCallDuration(seconds float64)
	IncToolCall(status string)
	ObserveContext(tokens, dropped int)
}

type Metrics struct {
	registry            *prometheus.Registry
	Runs                *prometheus.GaugeVec
	StepRetries         prometheus.Counter
	ModelFallbacks      prometheus.Counter
	ModelCallDuration   prometheus.Histogram
	ToolCalls           *prometheus.CounterVec
	ContextTokens       prometheus.Histogram
	ContextDroppedSteps prometheus.Counter
	TenantInflight      prometheus.Histogram
}

func NewMetrics() *Metrics {
	registry := prometheus.NewRegistry()
	m := &Metrics{
		registry: registry,
		Runs: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "platform_runs",
			Help: "Current number of runs by status.",
		}, []string{"status"}),
		StepRetries: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "platform_step_retries_total",
			Help: "Total number of step retries scheduled.",
		}),
		ModelFallbacks: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "platform_model_fallbacks_total",
			Help: "Total number of model calls that succeeded after a fallback.",
		}),
		ModelCallDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "platform_model_call_duration_seconds",
			Help:    "Latency of each model call attempt in seconds.",
			Buckets: prometheus.DefBuckets,
		}),
		ToolCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "platform_tool_calls_total",
			Help: "Total number of tool invocations by status.",
		}, []string{"status"}),
		ContextTokens: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "platform_context_tokens",
			Help:    "Estimated context tokens per ledger build.",
			Buckets: []float64{64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384, 32768},
		}),
		ContextDroppedSteps: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "platform_context_dropped_steps_total",
			Help: "Total number of ledger steps dropped from the context window.",
		}),
		TenantInflight: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "platform_tenant_inflight",
			Help:    "Distribution of in-flight runs per tenant.",
			Buckets: []float64{0, 1, 2, 3, 4, 6, 8, 12, 16, 24, 32},
		}),
	}
	m.ToolCalls.WithLabelValues("ok")
	m.ToolCalls.WithLabelValues("error")
	registry.MustRegister(
		m.Runs,
		m.StepRetries,
		m.ModelFallbacks,
		m.ModelCallDuration,
		m.ToolCalls,
		m.ContextTokens,
		m.ContextDroppedSteps,
		m.TenantInflight,
	)
	return m
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *Metrics) SetRunStatus(status string, count int) {
	m.Runs.WithLabelValues(status).Set(float64(count))
}

func (m *Metrics) ObserveTenantInflight(count int) {
	m.TenantInflight.Observe(float64(count))
}

func (m *Metrics) IncStepRetry() {
	m.StepRetries.Inc()
}

func (m *Metrics) IncModelFallback() {
	m.ModelFallbacks.Inc()
}

func (m *Metrics) ObserveModelCallDuration(seconds float64) {
	m.ModelCallDuration.Observe(seconds)
}

func (m *Metrics) IncToolCall(status string) {
	m.ToolCalls.WithLabelValues(status).Inc()
}

func (m *Metrics) ObserveContext(tokens, dropped int) {
	m.ContextTokens.Observe(float64(tokens))
	m.ContextDroppedSteps.Add(float64(dropped))
}
