package main

import (
	"testing"

	"github.com/chrisguidry/healthchecks-operator/healthchecks"
)

const cronUID = "cronjob-uid"

func TestPendingPingsIgnoresJobsOwnedByAnotherCronJob(t *testing.T) {
	cj := batchCronJob{Metadata: ObjectMeta{UID: cronUID}}
	jobs := []batchJob{
		job("other-1", "another-uid", at(0)).started(at(0)).succeeded(at(1)),
	}

	pings, runs := pendingPings(cj, jobs, cronJobRuns{})

	mustMatchPings(t, pings, nil)
	mustMatch(t, runs, cronJobRuns{})
}

func TestPendingPingsNewCheckReportsOnlyTheNewestFinishedJob(t *testing.T) {
	cj := batchCronJob{Metadata: ObjectMeta{UID: cronUID}}
	jobs := []batchJob{
		job("run-1", cronUID, at(0)).started(at(0)).succeeded(at(1)),
		job("run-2", cronUID, at(2)).started(at(2)).failed("BackoffLimitExceeded", "too many retries", at(3)),
	}

	pings, runs := pendingPings(cj, jobs, cronJobRuns{})

	// run-1 is older than the newest finished Job, so a new Check does
	// not replay it, and neither Job gets a start ping: both are
	// already finished, so a start ping now would never see a finish.
	mustMatchPings(t, pings, []jobPing{
		{job: "run-2", kind: healthchecks.PingFail, body: "run-2: BackoffLimitExceeded: too many retries"},
	})
	mustMatch(t, runs, cronJobRuns{lastReportedJob: "run-2"})
}

func TestPendingPingsNewCheckStillStartsAJobStillRunning(t *testing.T) {
	cj := batchCronJob{Metadata: ObjectMeta{UID: cronUID}}
	jobs := []batchJob{
		job("run-1", cronUID, at(0)).started(at(0)).succeeded(at(1)),
		job("run-2", cronUID, at(2)).started(at(2)), // still running
	}

	pings, runs := pendingPings(cj, jobs, cronJobRuns{})

	// run-1 already finished, so it is reported with no start ping.
	// run-2 has not finished, so it gets its start ping and no report.
	mustMatchPings(t, pings, []jobPing{
		{job: "run-1", kind: healthchecks.PingSuccess},
		{job: "run-2", kind: healthchecks.PingStart},
	})
	mustMatch(t, runs, cronJobRuns{lastStartedJob: "run-2", lastReportedJob: "run-1"})
}

func TestPendingPingsSteadyStateReportsOnlyNewFinishedJobs(t *testing.T) {
	cj := batchCronJob{Metadata: ObjectMeta{UID: cronUID}}
	jobs := []batchJob{
		job("run-1", cronUID, at(0)).started(at(0)).succeeded(at(1)),
		job("run-2", cronUID, at(2)).started(at(2)).succeeded(at(3)),
	}
	runs := cronJobRuns{lastStartedJob: "run-1", lastReportedJob: "run-1"}

	pings, next := pendingPings(cj, jobs, runs)

	mustMatchPings(t, pings, []jobPing{
		{job: "run-2", kind: healthchecks.PingStart},
		{job: "run-2", kind: healthchecks.PingSuccess},
	})
	mustMatch(t, next, cronJobRuns{lastStartedJob: "run-2", lastReportedJob: "run-2"})
}

func TestPendingPingsSendsNoDuplicateStartForAnAlreadyStartedJob(t *testing.T) {
	cj := batchCronJob{Metadata: ObjectMeta{UID: cronUID}}
	jobs := []batchJob{
		job("run-1", cronUID, at(0)).started(at(0)), // still running
	}
	runs := cronJobRuns{lastStartedJob: "run-1"}

	pings, next := pendingPings(cj, jobs, runs)

	mustMatchPings(t, pings, nil)
	mustMatch(t, next, runs)
}

func TestPendingPingsStartsAFinishedJobThatMissedItsStartPing(t *testing.T) {
	// lastStartedJob is memory-only, so a restart forgets it even
	// though lastReportedJob (persisted) still names an earlier Job.
	// The Job that finished since then gets its start ping too, right
	// before its finish ping, so Healthchecks can still time the run.
	cj := batchCronJob{Metadata: ObjectMeta{UID: cronUID}}
	jobs := []batchJob{
		job("run-1", cronUID, at(0)).started(at(0)).succeeded(at(1)),
		job("run-2", cronUID, at(2)).started(at(2)).succeeded(at(3)),
	}
	runs := cronJobRuns{lastReportedJob: "run-1"}

	pings, next := pendingPings(cj, jobs, runs)

	mustMatchPings(t, pings, []jobPing{
		{job: "run-2", kind: healthchecks.PingStart},
		{job: "run-2", kind: healthchecks.PingSuccess},
	})
	mustMatch(t, next, cronJobRuns{lastStartedJob: "run-2", lastReportedJob: "run-2"})
}

func TestPendingPingsFallsBackToNewestWhenLastReportedJobIsPruned(t *testing.T) {
	// run-0 is gone: Kubernetes pruned it past the history limit before
	// the operator's next pass. cronJobRuns keeps no timestamp for it,
	// so there is nothing left to place its old position by, and the
	// operator falls back to the same newest-only catch-up a new Check
	// gets.
	cj := batchCronJob{Metadata: ObjectMeta{UID: cronUID}}
	jobs := []batchJob{
		job("run-1", cronUID, at(0)).started(at(0)).succeeded(at(1)),
		job("run-2", cronUID, at(2)).started(at(2)).succeeded(at(3)),
	}
	runs := cronJobRuns{lastStartedJob: "run-0", lastReportedJob: "run-0"}

	pings, next := pendingPings(cj, jobs, runs)

	mustMatchPings(t, pings, []jobPing{
		{job: "run-2", kind: healthchecks.PingSuccess},
	})
	mustMatch(t, next, cronJobRuns{lastStartedJob: "run-0", lastReportedJob: "run-2"})
}

func TestPendingPingsOrdersJobsByCreationThenName(t *testing.T) {
	cj := batchCronJob{Metadata: ObjectMeta{UID: cronUID}}
	// Both created at the same instant; the name breaks the tie.
	jobs := []batchJob{
		job("run-b", cronUID, at(0)).started(at(0)).succeeded(at(1)),
		job("run-a", cronUID, at(0)).started(at(0)).succeeded(at(1)),
	}

	pings, _ := pendingPings(cj, jobs, cronJobRuns{lastStartedJob: "run-a", lastReportedJob: "run-a"})

	// run-a sorts first and is already tracked, so only run-b is new;
	// it gets its start ping before its success, since neither was
	// sent for it yet.
	mustMatchPings(t, pings, []jobPing{
		{job: "run-b", kind: healthchecks.PingStart},
		{job: "run-b", kind: healthchecks.PingSuccess},
	})
}

func TestPendingPingsReturnsPingsInTheOrderTheyHappened(t *testing.T) {
	cj := batchCronJob{Metadata: ObjectMeta{UID: cronUID}}
	jobs := []batchJob{
		job("run-1", cronUID, at(0)).started(at(0)).succeeded(at(1)),
		job("run-2", cronUID, at(2)).started(at(2)), // still running
	}
	runs := cronJobRuns{lastStartedJob: "run-1", lastReportedJob: "run-1"}

	pings, next := pendingPings(cj, jobs, runs)

	mustMatchPings(t, pings, []jobPing{
		{job: "run-2", kind: healthchecks.PingStart},
	})
	mustMatch(t, next, cronJobRuns{lastStartedJob: "run-2", lastReportedJob: "run-1"})
}
