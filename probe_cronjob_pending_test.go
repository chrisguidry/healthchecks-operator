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
		{job: "run-2", created: at(2), kind: healthchecks.PingFail, body: "run-2: BackoffLimitExceeded: too many retries"},
	})
	mustMatch(t, runs, cronJobRuns{lastReportedJob: "run-2", lastReportedCreated: at(2), lastReportedOwner: cronUID})
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
		{job: "run-1", created: at(0), kind: healthchecks.PingSuccess},
		{job: "run-2", created: at(2), kind: healthchecks.PingStart},
	})
	mustMatch(t, runs, cronJobRuns{lastStartedJob: "run-2", lastReportedJob: "run-1", lastReportedCreated: at(0), lastReportedOwner: cronUID})
}

func TestPendingPingsSteadyStateReportsOnlyNewFinishedJobs(t *testing.T) {
	cj := batchCronJob{Metadata: ObjectMeta{UID: cronUID}}
	jobs := []batchJob{
		job("run-1", cronUID, at(0)).started(at(0)).succeeded(at(1)),
		job("run-2", cronUID, at(2)).started(at(2)).succeeded(at(3)),
	}
	runs := cronJobRuns{lastStartedJob: "run-1", lastReportedJob: "run-1", lastReportedCreated: at(0), lastReportedOwner: cronUID}

	pings, next := pendingPings(cj, jobs, runs)

	mustMatchPings(t, pings, []jobPing{
		{job: "run-2", created: at(2), kind: healthchecks.PingStart},
		{job: "run-2", created: at(2), kind: healthchecks.PingSuccess},
	})
	mustMatch(t, next, cronJobRuns{lastStartedJob: "run-2", lastReportedJob: "run-2", lastReportedCreated: at(2), lastReportedOwner: cronUID})
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
	runs := cronJobRuns{lastReportedJob: "run-1", lastReportedCreated: at(0), lastReportedOwner: cronUID}

	pings, next := pendingPings(cj, jobs, runs)

	mustMatchPings(t, pings, []jobPing{
		{job: "run-2", created: at(2), kind: healthchecks.PingStart},
		{job: "run-2", created: at(2), kind: healthchecks.PingSuccess},
	})
	mustMatch(t, next, cronJobRuns{lastStartedJob: "run-2", lastReportedJob: "run-2", lastReportedCreated: at(2), lastReportedOwner: cronUID})
}

func TestPendingPingsReportsNothingAgainWhenTheLastReportedJobIsDeleted(t *testing.T) {
	// run-2 was a manual run that fixed run-1's failure, and then someone
	// deleted it. run-1 sorts before run-2's place, so it was reported
	// already, and its failure is not sent a second time.
	cj := batchCronJob{Metadata: ObjectMeta{UID: cronUID}}
	jobs := []batchJob{
		job("run-1", cronUID, at(0)).started(at(0)).failed("BackoffLimitExceeded", "too many retries", at(1)),
	}
	runs := cronJobRuns{lastStartedJob: "run-2", lastReportedJob: "run-2", lastReportedCreated: at(2), lastReportedOwner: cronUID}

	pings, next := pendingPings(cj, jobs, runs)

	mustMatchPings(t, pings, nil)
	mustMatch(t, next, runs)
}

func TestPendingPingsReportsEveryJobAfterAPrunedLastReportedJob(t *testing.T) {
	// run-0 is gone: Kubernetes pruned it past the history limit before
	// the operator's next pass. Every Job made after it is new.
	cj := batchCronJob{Metadata: ObjectMeta{UID: cronUID}}
	jobs := []batchJob{
		job("run-1", cronUID, at(1)).started(at(1)).succeeded(at(2)),
		job("run-2", cronUID, at(2)).started(at(2)).succeeded(at(3)),
	}
	runs := cronJobRuns{lastStartedJob: "run-0", lastReportedJob: "run-0", lastReportedCreated: at(0), lastReportedOwner: cronUID}

	pings, next := pendingPings(cj, jobs, runs)

	mustMatchPings(t, pings, []jobPing{
		{job: "run-1", created: at(1), kind: healthchecks.PingStart},
		{job: "run-1", created: at(1), kind: healthchecks.PingSuccess},
		{job: "run-2", created: at(2), kind: healthchecks.PingStart},
		{job: "run-2", created: at(2), kind: healthchecks.PingSuccess},
	})
	mustMatch(t, next, cronJobRuns{lastStartedJob: "run-2", lastReportedJob: "run-2", lastReportedCreated: at(2), lastReportedOwner: cronUID})
}

func TestPendingPingsReportsAJobMadeAgainUnderTheLastReportedName(t *testing.T) {
	// Someone deleted run-1 and ran it again by hand under the same name.
	// The new run-1 was made after the one the operator reported, so it
	// is a new run.
	cj := batchCronJob{Metadata: ObjectMeta{UID: cronUID}}
	jobs := []batchJob{
		job("run-1", cronUID, at(5)).started(at(5)).succeeded(at(6)),
	}
	runs := cronJobRuns{lastReportedJob: "run-1", lastReportedCreated: at(0), lastReportedOwner: cronUID}

	pings, next := pendingPings(cj, jobs, runs)

	mustMatchPings(t, pings, []jobPing{
		{job: "run-1", created: at(5), kind: healthchecks.PingStart},
		{job: "run-1", created: at(5), kind: healthchecks.PingSuccess},
	})
	mustMatch(t, next, cronJobRuns{lastStartedJob: "run-1", lastReportedJob: "run-1", lastReportedCreated: at(5), lastReportedOwner: cronUID})
}

func TestPendingPingsIgnoresThePlaceOfAnotherCronJob(t *testing.T) {
	// The Check named another CronJob when it reported x-9. That place
	// says nothing about this CronJob's Jobs, so this CronJob gets the
	// newest-only catch-up of a new Check, and run-1's failure is not
	// sent to a check that never reported run-1.
	cj := batchCronJob{Metadata: ObjectMeta{UID: cronUID}}
	jobs := []batchJob{
		job("run-1", cronUID, at(1)).started(at(1)).failed("BackoffLimitExceeded", "too many retries", at(2)),
		job("run-2", cronUID, at(2)).started(at(2)).succeeded(at(3)),
	}
	runs := cronJobRuns{lastReportedJob: "x-9", lastReportedCreated: at(0), lastReportedOwner: "another-uid"}

	pings, next := pendingPings(cj, jobs, runs)

	mustMatchPings(t, pings, []jobPing{
		{job: "run-2", created: at(2), kind: healthchecks.PingSuccess},
	})
	mustMatch(t, next, cronJobRuns{lastReportedJob: "run-2", lastReportedCreated: at(2), lastReportedOwner: cronUID})
}

// A status written before lastReportedJobCreated existed holds only the
// Job's name. While that Job is still there, reporting resumes after it.
// Once it is gone, nothing places its old position, so the operator
// reports the newest finished Job alone, as it does for a new Check.
func TestPendingPingsPlacesAStatusWithOnlyAName(t *testing.T) {
	cases := []struct {
		name string
		last string
		want []jobPing
	}{
		{"still there", "run-1", []jobPing{
			{job: "run-2", created: at(2), kind: healthchecks.PingStart},
			{job: "run-2", created: at(2), kind: healthchecks.PingSuccess},
		}},
		{"gone", "run-0", []jobPing{
			{job: "run-2", created: at(2), kind: healthchecks.PingSuccess},
		}},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			cj := batchCronJob{Metadata: ObjectMeta{UID: cronUID}}
			jobs := []batchJob{
				job("run-1", cronUID, at(0)).started(at(0)).succeeded(at(1)),
				job("run-2", cronUID, at(2)).started(at(2)).succeeded(at(3)),
			}

			pings, next := pendingPings(cj, jobs, cronJobRuns{lastReportedJob: one.last})

			mustMatchPings(t, pings, one.want)
			mustMatch(t, next.lastReportedCreated, at(2))
		})
	}
}

// A status with only a name gets the Job's creation time on the first
// pass, with no new run, so the place survives that Job's deletion.
func TestPendingPingsFillsInTheCreationTimeOfAStatusWithOnlyAName(t *testing.T) {
	cj := batchCronJob{Metadata: ObjectMeta{UID: cronUID}}
	jobs := []batchJob{
		job("run-1", cronUID, at(0)).started(at(0)).succeeded(at(1)),
	}

	pings, next := pendingPings(cj, jobs, cronJobRuns{lastReportedJob: "run-1"})

	mustMatchPings(t, pings, nil)
	mustMatch(t, next, cronJobRuns{lastReportedJob: "run-1", lastReportedCreated: at(0), lastReportedOwner: cronUID})
}

func TestPendingPingsOrdersJobsByCreationThenName(t *testing.T) {
	cj := batchCronJob{Metadata: ObjectMeta{UID: cronUID}}
	// Both created at the same instant; the name breaks the tie.
	jobs := []batchJob{
		job("run-b", cronUID, at(0)).started(at(0)).succeeded(at(1)),
		job("run-a", cronUID, at(0)).started(at(0)).succeeded(at(1)),
	}

	pings, _ := pendingPings(cj, jobs, cronJobRuns{lastStartedJob: "run-a", lastReportedJob: "run-a", lastReportedCreated: at(0), lastReportedOwner: cronUID})

	// run-a sorts first and is already tracked, so only run-b is new;
	// it gets its start ping before its success, since neither was
	// sent for it yet.
	mustMatchPings(t, pings, []jobPing{
		{job: "run-b", created: at(0), kind: healthchecks.PingStart},
		{job: "run-b", created: at(0), kind: healthchecks.PingSuccess},
	})
}

func TestPendingPingsReturnsPingsInTheOrderTheyHappened(t *testing.T) {
	cj := batchCronJob{Metadata: ObjectMeta{UID: cronUID}}
	jobs := []batchJob{
		job("run-1", cronUID, at(0)).started(at(0)).succeeded(at(1)),
		job("run-2", cronUID, at(2)).started(at(2)), // still running
	}
	runs := cronJobRuns{lastStartedJob: "run-1", lastReportedJob: "run-1", lastReportedCreated: at(0), lastReportedOwner: cronUID}

	pings, next := pendingPings(cj, jobs, runs)

	mustMatchPings(t, pings, []jobPing{
		{job: "run-2", created: at(2), kind: healthchecks.PingStart},
	})
	mustMatch(t, next, cronJobRuns{lastStartedJob: "run-2", lastReportedJob: "run-1", lastReportedCreated: at(0), lastReportedOwner: cronUID})
}
