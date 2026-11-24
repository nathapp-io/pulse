// Package metrics exposes the prober's Prometheus surface.
//
// This is deliberately independent of the push path: Prometheus (later) scrapes
// the prober directly, while the viewer only ever receives pushed results.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"pulse/internal/build"
	"pulse/internal/probe"
)

// Metrics owns the prober's collectors and registry.
type Metrics struct {
	reg *prometheus.Registry

	success     *prometheus.GaugeVec
	duration    *prometheus.GaugeVec
	statusCode  *prometheus.GaugeVec
	tlsExpiry   *prometheus.GaugeVec
	lastSuccess *prometheus.GaugeVec
	errorTotal  *prometheus.CounterVec
	uptimeRatio *prometheus.GaugeVec

	pushQueued      prometheus.Gauge
	pushDropped     prometheus.Counter
	pushFailures    prometheus.Counter
	pushLastSuccess prometheus.Gauge
}

// New builds and registers the collectors.
func New() *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		reg: reg,
		success: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_success",
			Help: "1 if the last probe of this target succeeded, 0 otherwise.",
		}, []string{"target", "group", "type"}),
		duration: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_duration_seconds",
			Help: "Duration of the last probe by phase (dns, connect, tls, ttfb, total).",
		}, []string{"target", "group", "type", "phase"}),
		statusCode: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_http_status_code",
			Help: "HTTP status code of the last probe (0 for non-HTTP targets).",
		}, []string{"target", "group", "type"}),
		tlsExpiry: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_tls_expiry_seconds",
			Help: "Certificate not-after of the last probe, as a unix timestamp.",
		}, []string{"target", "group", "type"}),
		lastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_last_success_timestamp_seconds",
			Help: "Unix timestamp of the last successful probe.",
		}, []string{"target", "group", "type"}),
		errorTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "probe_error_total",
			Help: "Probe failures by error class.",
		}, []string{"target", "group", "type", "class"}),
		uptimeRatio: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_uptime_ratio",
			Help: "Successful probe ratio over the window (1h, 24h, 7d).",
		}, []string{"target", "group", "window"}),
		pushQueued: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "pulse_push_queued",
			Help: "Results waiting to be pushed to the viewer.",
		}),
		pushDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "pulse_push_dropped_total",
			Help: "Results dropped because the push queue was full.",
		}),
		pushFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "pulse_push_failures_total",
			Help: "Failed push attempts (retried with backoff; results stay queued).",
		}),
		pushLastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "pulse_push_last_success_timestamp_seconds",
			Help: "Unix timestamp of the last successful push to the viewer.",
		}),
	}

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pulse_build_info",
		Help: "Build information, always 1.",
	}, []string{"role", "version"})
	buildInfo.WithLabelValues("prober", build.Version).Set(1)

	reg.MustRegister(
		m.success, m.duration, m.statusCode, m.tlsExpiry, m.lastSuccess,
		m.errorTotal, m.uptimeRatio,
		m.pushQueued, m.pushDropped, m.pushFailures, m.pushLastSuccess, buildInfo,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Observe records one probe result.
func (m *Metrics) Observe(r probe.Result) {
	labels := prometheus.Labels{"target": r.Target, "group": r.Group, "type": r.Type}

	if r.OK {
		m.success.With(labels).Set(1)
		m.lastSuccess.With(labels).Set(float64(r.At.Unix()))
	} else {
		m.success.With(labels).Set(0)
		m.errorTotal.With(prometheus.Labels{
			"target": r.Target, "group": r.Group, "type": r.Type, "class": string(r.ErrorClass),
		}).Inc()
	}

	for phase, ms := range r.PhaseMS {
		m.duration.With(prometheus.Labels{
			"target": r.Target, "group": r.Group, "type": r.Type, "phase": string(phase),
		}).Set(float64(ms) / 1000)
	}

	m.statusCode.With(labels).Set(float64(r.StatusCode))
	if !r.TLSExpiry.IsZero() {
		m.tlsExpiry.With(labels).Set(float64(r.TLSExpiry.Unix()))
	}
}

// SetUptime records a rolling uptime ratio. A negative ratio means "no data yet"
// and is not exported, so the series stays absent rather than falsely zero.
func (m *Metrics) SetUptime(target, group, window string, ratio float64) {
	if ratio < 0 {
		return
	}
	m.uptimeRatio.With(prometheus.Labels{
		"target": target, "group": group, "window": window,
	}).Set(ratio)
}

// PushQueued reports how many results are waiting to be pushed.
func (m *Metrics) PushQueued(n int) { m.pushQueued.Set(float64(n)) }

// PushDropped records one result dropped from a full queue.
func (m *Metrics) PushDropped() { m.pushDropped.Inc() }

// PushFailed records one failed push attempt.
func (m *Metrics) PushFailed() { m.pushFailures.Inc() }

// PushSucceeded records a successful push batch at time t.
func (m *Metrics) PushSucceeded(t time.Time) { m.pushLastSuccess.Set(float64(t.Unix())) }

// Handler serves the registry in Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}
