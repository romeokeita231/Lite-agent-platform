package observability

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

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
			Buckets: prometheus.DefBuckets,
		}),
		ContextDroppedSteps: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "platform_context_dropped_steps_total",
			Help: "Total number of ledger steps dropped from the context window.",
		}),
		TenantInflight: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "platform_tenant_inflight",
			Help:    "Distribution of in-flight runs per tenant.",
			Buckets: prometheus.DefBuckets,
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
