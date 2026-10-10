package main

// A probe checks one thing, an HTTP endpoint or a TLS certificate, and
// returns pass or fail with a reason. It never calls Healthchecks: the
// check_*.go files send the pings. probe_http.go and probe_tls.go build
// the probers, and probe_schedule.go runs them on their intervals.

import (
	"context"
	"fmt"
	"strings"
)

// probeResult is the outcome of one probe run. reason is empty when the
// probe passed, and it is the ping body when the probe failed.
type probeResult struct {
	passed bool
	reason string
}

// resultOf is the result of a run over total targets, of which each
// line in failed names one that failed. A probe tries every target, so
// the reason tells "only the second address fails" from "everything
// fails". One failure is the whole reason, so a probe with one target
// reads the same as before. Several start with a count, then take one
// line each.
func resultOf(failed []string, total int, noun string) probeResult {
	switch len(failed) {
	case 0:
		return probeResult{passed: true}
	case 1:
		return probeResult{reason: failed[0]}
	}
	return probeResult{reason: fmt.Sprintf("%d of %d %s failed\n%s", len(failed), total, noun, strings.Join(failed, "\n"))}
}

// prober runs one probe. It never calls Healthchecks.
type prober interface {
	probe(ctx context.Context) probeResult
}

// secretReader reads one key of one Secret. *kubeClient implements it
// in kube_secret.go.
type secretReader interface {
	secretValue(ctx context.Context, namespace, name, key string) (string, error)
}
