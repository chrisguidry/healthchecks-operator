package main

// A cronJob Check has no probe. Each pass compares the CronJob's Jobs
// with where the reporting stands, and sends a ping for each change in
// order: /start when a Job starts, success when it completes, and
// /fail when it fails. A Job that finished while the operator was down
// still gets its ping on the first pass.

import (
	"context"

	"github.com/chrisguidry/healthchecks-operator/healthchecks"
)

// reportRuns sends the pings the CronJob's Jobs call for. It stops at
// the first ping that fails, and state.runs holds the pings sent before
// it. The failure puts the Check on a backoff, and the first pass after
// it sends the rest in order, and nothing twice.
func (c *controller) reportRuns(ctx context.Context, state *checkState, cronJob batchCronJob, jobs []batchJob, pingURL string) {
	pings, after := pendingPings(cronJob, jobs, state.runs)
	for _, ping := range pings {
		err := healthchecks.Ping(ctx, c.http, pingURL, ping.kind, ping.body)
		c.readings.observePing(pingLabel(ping.kind), err)
		if err != nil {
			c.log.fault(state.subject(), topicPing, "pinging for Job "+ping.job+": "+errorText(err))
			state.backoff.failed(c.now())
			return
		}
		c.log.clear(state.subject(), topicPing)
		if ping.kind == healthchecks.PingStart {
			state.runs.lastStartedJob = ping.job
		} else {
			state.runs.lastReportedJob = ping.job
		}
	}
	state.runs = after
}

// pingLabel is the pings_total label for a kind of ping.
func pingLabel(kind healthchecks.PingKind) string {
	switch kind {
	case healthchecks.PingStart:
		return pingStart
	case healthchecks.PingFail:
		return pingFail
	}
	return pingSuccess
}
