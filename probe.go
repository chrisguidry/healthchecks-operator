package main

// A probe checks one thing, an HTTP endpoint or a TLS certificate, and
// returns pass or fail with a reason. It never calls Healthchecks: the
// check_*.go files send the pings. probe_http.go and probe_tls.go build
// the probers, and probe_schedule.go runs them on their intervals.

import "context"

// probeResult is the outcome of one probe run. reason is empty when the
// probe passed, and it is the ping body when the probe failed.
type probeResult struct {
	passed bool
	reason string
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
