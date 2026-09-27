package main

// A cronJob Check has no probe. Each pass compares the CronJob's Jobs
// with where the reporting stands, and sends a ping for each change in
// order: /start when a Job starts, success when it completes, and
// /fail when it fails. A Job that finished while the operator was down
// still gets its ping on the first pass.

import (
	"context"
	"time"

	"github.com/chrisguidry/healthchecks-operator/healthchecks"
)

// reportRuns sends the pings the CronJob's Jobs call for, and returns
// the last success or failure it sent, or nil when it sent none. It
// stops at the first ping that fails, and state.runs holds the pings
// sent before it. The failure puts the Check on a backoff, and the
// first pass after it sends the rest in order, and nothing twice.
func (c *controller) reportRuns(ctx context.Context, state *checkState, cronJob batchCronJob, jobs []batchJob, pingURL string) *jobPing {
	pings, after := pendingPings(cronJob, jobs, state.runs)
	var finished *jobPing
	for _, ping := range pings {
		err := healthchecks.Ping(ctx, c.http, pingURL, ping.kind, ping.body)
		c.readings.observePing(pingLabel(ping.kind), err)
		if err != nil {
			c.log.fault(state.subject(), topicPing, "pinging for Job "+ping.job+": "+errorText(err))
			state.backoff.failed(c.now())
			return finished
		}
		c.log.clear(state.subject(), topicPing)
		if ping.kind == healthchecks.PingStart {
			state.runs.lastStartedJob = ping.job
		} else {
			state.runs.lastReportedJob = ping.job
			finished = &ping
		}
	}
	state.runs = after
	return finished
}

// The reasons a cronJob Check's Passing condition gives. They differ
// from a probe's, so a Check that changes from a probe to a CronJob
// drops the probe's Passing instead of showing it as the CronJob's.
const (
	reasonRunSucceeded = "RunSucceeded"
	reasonRunFailed    = "RunFailed"
)

// runPassing is the Passing condition for the last run the operator
// reported. A failure's message is the ping body, which names the Job
// and says why it failed.
func runPassing(finished jobPing, generation int64, now time.Time) Condition {
	condition := Condition{
		Type:               passingCondition,
		Status:             ConditionTrue,
		ObservedGeneration: generation,
		Reason:             reasonRunSucceeded,
		Message:            finished.job + " completed",
		LastTransitionTime: timestamp(now),
	}
	if finished.kind == healthchecks.PingFail {
		condition.Status, condition.Reason, condition.Message = ConditionFalse, reasonRunFailed, finished.body
	}
	return condition
}

// fromRun says whether the Passing condition in conditions came from a
// reported run, and not from a probe the Check ran before.
func fromRun(conditions []Condition) bool {
	passing, found := findCondition(conditions, passingCondition)
	return found && (passing.Reason == reasonRunSucceeded || passing.Reason == reasonRunFailed)
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
