package main

import (
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// scrape reads the metrics the way Prometheus does, over HTTP, and
// returns the sample lines without the comments.
func scrape(t *testing.T, m *metrics) []string {
	t.Helper()
	address, err := m.serve(t.Context(), "127.0.0.1:0")
	mustSucceed(t, err)
	resp, err := http.Get("http://" + address.String() + "/metrics")
	mustSucceed(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	mustSucceed(t, err)
	var samples []string
	for line := range strings.Lines(string(body)) {
		if !strings.HasPrefix(line, "#") {
			samples = append(samples, strings.TrimSpace(line))
		}
	}
	return samples
}

func TestEveryCounterStartsAtZero(t *testing.T) {
	samples := scrape(t, newMetrics("2026.09.27-001"))

	for _, want := range []string{
		`healthchecks_build_info{version="2026.09.27-001"} 1`,
		`healthchecks_reconcile_duration_seconds_count 0`,
		`healthchecks_reconcile_errors_total{kind="Check"} 0`,
		`healthchecks_reconcile_errors_total{kind="ClusterProject"} 0`,
		`healthchecks_watch_restarts_total{resource="jobs"} 0`,
		`healthchecks_watch_restarts_total{resource="cronjobs"} 0`,
		`healthchecks_probes_total{probe="tls",result="fail"} 0`,
		`healthchecks_pings_total{outcome="error",ping="start"} 0`,
	} {
		t.Run(want, func(t *testing.T) {
			mustMatch(t, slices.Contains(samples, want), true)
		})
	}
}

func TestEachObservationMovesItsSeries(t *testing.T) {
	m := newMetrics("dev")
	m.observePass(5 * time.Millisecond)
	m.reconcileFailed(checkKind)
	m.watchRestarted("checks")()
	m.observeProbe(probeHTTP, false, 30*time.Millisecond)
	m.observePing(pingFail, nil)
	m.observePing(pingSuccess, errors.New("connection refused"))

	samples := scrape(t, m)

	for _, want := range []string{
		`healthchecks_reconcile_duration_seconds_count 1`,
		`healthchecks_reconcile_errors_total{kind="Check"} 1`,
		`healthchecks_watch_restarts_total{resource="checks"} 1`,
		`healthchecks_probe_duration_seconds_count{probe="http"} 1`,
		`healthchecks_probes_total{probe="http",result="fail"} 1`,
		`healthchecks_pings_total{outcome="ok",ping="fail"} 1`,
		`healthchecks_pings_total{outcome="error",ping="success"} 1`,
	} {
		t.Run(want, func(t *testing.T) {
			mustMatch(t, slices.Contains(samples, want), true)
		})
	}
}

func TestMetricsServeFailsOnAPortInUse(t *testing.T) {
	m := newMetrics("dev")
	address, err := m.serve(t.Context(), "127.0.0.1:0")
	mustSucceed(t, err)

	_, err = m.serve(t.Context(), address.String())

	mustMatch(t, err != nil, true)
}
