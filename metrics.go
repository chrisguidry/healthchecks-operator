package main

// The operator's metrics are on a private registry, so a scrape reads
// only what this file registers. Every series is set when the fact it
// reports happens, and a scrape reads memory only.

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const metricsPrefix = "healthchecks_"

// The kinds a reconcile error is counted by.
const (
	clusterProjectKind = "ClusterProject"
	checkKind          = "Check"
)

// The probe kinds that run on the timer. A cronJob check has no probe:
// it reports Jobs that already ran.
const (
	probeHTTP = "http"
	probeTLS  = "tls"
)

// The pings a check sends to Healthchecks. The operator's own heartbeat
// counts as a success ping.
const (
	pingSuccess = "success"
	pingStart   = "start"
	pingFail    = "fail"
)

// The resources the operator watches, by the name that labels their
// restarts.
var watchedResources = []string{"clusterprojects", "checks", "cronjobs", "jobs"}

type metrics struct {
	registry *prometheus.Registry

	passDuration    prometheus.Histogram
	reconcileErrors *prometheus.CounterVec
	watchRestarts   *prometheus.CounterVec
	probeDuration   *prometheus.HistogramVec
	probes          *prometheus.CounterVec
	pings           *prometheus.CounterVec
}

func newMetrics(version string) *metrics {
	m := &metrics{
		registry: prometheus.NewRegistry(),
		passDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: metricsPrefix + "reconcile_duration_seconds",
			Help: "How long one reconcile pass over every ClusterProject and Check takes.",
			// A pass with nothing to do takes about a millisecond. A pass
			// that calls the Healthchecks API takes the length of a few
			// requests.
			Buckets: prometheus.ExponentialBuckets(0.001, 4, 8),
		}),
		reconcileErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricsPrefix + "reconcile_errors_total",
			Help: "Objects whose reconcile failed, by kind.",
		}, []string{"kind"}),
		watchRestarts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricsPrefix + "watch_restarts_total",
			Help: "Watches that ended and opened again after a list, by resource.",
		}, []string{"resource"}),
		probeDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    metricsPrefix + "probe_duration_seconds",
			Help:    "How long one probe takes, by probe kind.",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 12),
		}, []string{"probe"}),
		probes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricsPrefix + "probes_total",
			Help: "Probes run, by probe kind and result.",
		}, []string{"probe", "result"}),
		pings: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricsPrefix + "pings_total",
			Help: "Pings sent to Healthchecks, by ping and by whether Healthchecks accepted it.",
		}, []string{"ping", "outcome"}),
	}

	// build_info always holds 1, and its label is the release. This is
	// the Prometheus convention for a string value.
	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: metricsPrefix + "build_info",
		Help: "The release this operator runs, as a label on a gauge that is always 1.",
	}, []string{"version"})
	buildInfo.WithLabelValues(version).Set(1)

	m.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		buildInfo,
		m.passDuration, m.reconcileErrors, m.watchRestarts,
		m.probeDuration, m.probes, m.pings,
	)

	// Every series starts at zero on the first scrape. Prometheus
	// computes a rate from two readings, and a counter that first
	// appears at 1 has no earlier reading.
	for _, kind := range []string{clusterProjectKind, checkKind} {
		m.reconcileErrors.WithLabelValues(kind)
	}
	for _, resource := range watchedResources {
		m.watchRestarts.WithLabelValues(resource)
	}
	for _, probe := range []string{probeHTTP, probeTLS} {
		m.probeDuration.WithLabelValues(probe)
		m.probes.WithLabelValues(probe, "pass")
		m.probes.WithLabelValues(probe, "fail")
	}
	for _, ping := range []string{pingSuccess, pingStart, pingFail} {
		m.pings.WithLabelValues(ping, "ok")
		m.pings.WithLabelValues(ping, "error")
	}
	return m
}

// observePass records one reconcile pass. Every pass counts, including
// one that changed nothing, because the rate of passes shows that the
// loop runs.
func (m *metrics) observePass(took time.Duration) {
	m.passDuration.Observe(took.Seconds())
}

// reconcileFailed counts one object of kind whose reconcile failed.
func (m *metrics) reconcileFailed(kind string) {
	m.reconcileErrors.WithLabelValues(kind).Inc()
}

// watchRestarted returns the function a collectionWatch calls on each
// restart of the watch on resource.
func (m *metrics) watchRestarted(resource string) func() {
	counter := m.watchRestarts.WithLabelValues(resource)
	return counter.Inc
}

func (m *metrics) observeProbe(probe string, passed bool, took time.Duration) {
	m.probeDuration.WithLabelValues(probe).Observe(took.Seconds())
	m.probes.WithLabelValues(probe, outcomeLabel(passed, "pass", "fail")).Inc()
}

// observePing counts one ping, and whether Healthchecks accepted it.
func (m *metrics) observePing(ping string, err error) {
	m.pings.WithLabelValues(ping, outcomeLabel(err == nil, "ok", "error")).Inc()
}

func outcomeLabel(good bool, yes, no string) string {
	if good {
		return yes
	}
	return no
}

func (m *metrics) handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		// A collector that fails breaks the scrape, so Prometheus does
		// not store a document with a gap in it.
		ErrorHandling: promhttp.HTTPErrorOnError,
	})
}

// serve listens on address and serves /metrics until ctx ends. It
// listens before it returns, so a port that is in use is an error for
// the caller, and it returns the address it listens on.
func (m *metrics) serve(ctx context.Context, address string) (net.Addr, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.handler())
	// A client that connects and sends no headers holds a goroutine
	// until this timeout ends it.
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	context.AfterFunc(ctx, func() { _ = server.Close() })
	go func() { _ = server.Serve(listener) }()
	return listener.Addr(), nil
}
