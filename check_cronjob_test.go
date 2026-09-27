package main

import (
	"testing"
	"time"
)

// backupCronJob is the CronJob that the cronJob checks here report.
func backupCronJob() map[string]any {
	return map[string]any{
		"metadata": map[string]any{"namespace": "example", "name": "database-backup"},
		"spec":     map[string]any{"schedule": "@daily", "timeZone": "America/New_York"},
	}
}

func backupCheck() Check {
	return Check{
		Metadata: ObjectMeta{Namespace: "example", Name: "database-backup"},
		Spec: CheckSpec{
			ProjectRef:  ProjectRef{Kind: "ClusterProject", Name: "internal"},
			Description: "Nightly database backup",
			CronJob:     &CronJobProbe{Name: "database-backup"},
		},
	}
}

func TestACronJobCheckCopiesTheSchedule(t *testing.T) {
	h := startHarness(t)
	h.api.create(cronJobsResource, backupCronJob())
	h.api.create(checksResource, backupCheck())

	h.pass()

	stored, _ := h.hc.Check("example-database-backup")
	mustMatch(t, stored.Schedule, "0 0 * * *")
	mustMatch(t, stored.Timezone, "America/New_York")
	mustMatch(t, stored.Timeout, time.Duration(0))
	check := h.check("example", "database-backup")
	mustMatch(t, conditionOf(check, "Ready").Status, ConditionTrue)
	mustMatch(t, conditionOf(check, "Passing").Status, "")
}

func TestAChangedCronJobScheduleUpdatesTheCheck(t *testing.T) {
	h := startHarness(t)
	h.api.create(cronJobsResource, backupCronJob())
	h.api.create(checksResource, backupCheck())
	h.pass()

	h.api.update(cronJobsResource, "example", "database-backup", func(object map[string]any) {
		object["spec"].(map[string]any)["schedule"] = "30 2 * * ?"
	})
	h.pass()

	stored, _ := h.hc.Check("example-database-backup")
	mustMatch(t, stored.Schedule, "30 2 * * *")
}

func TestACronJobCheckIsNotReadyWithoutAUsableCronJob(t *testing.T) {
	cases := []struct {
		name    string
		cronJob map[string]any
		reason  string
		message string
	}{
		{
			"missing",
			map[string]any{
				"metadata": map[string]any{"namespace": "example", "name": "nightly-report"},
				"spec":     map[string]any{"schedule": "@daily"},
			},
			"CronJobNotFound",
			"CronJob example/database-backup does not exist",
		},
		{
			"keeping no failed Job",
			map[string]any{
				"metadata": map[string]any{"namespace": "example", "name": "database-backup"},
				"spec":     map[string]any{"schedule": "@daily", "failedJobsHistoryLimit": 0},
			},
			"CronJobHistory",
			"failedJobsHistoryLimit is 0: the operator needs one finished Job kept to find a run that finished while it was down",
		},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			h := startHarness(t)
			h.api.create(cronJobsResource, one.cronJob)
			h.api.create(checksResource, backupCheck())

			h.pass()

			ready := conditionOf(h.check("example", "database-backup"), "Ready")
			mustMatch(t, ready.Reason, one.reason)
			mustMatch(t, ready.Message, one.message)
		})
	}
}

// startRun creates a running Job that the backup CronJob owns.
func (h *harness) startRun(name string) {
	h.t.Helper()
	var cronJob batchCronJob
	h.api.read(cronJobsResource, "example", "database-backup", &cronJob)
	h.api.create(jobsResource, map[string]any{
		"metadata": map[string]any{
			"namespace":       "example",
			"name":            name,
			"ownerReferences": []any{map[string]any{"kind": "CronJob", "name": "database-backup", "uid": cronJob.Metadata.UID}},
		},
		"status": map[string]any{"startTime": "2026-09-27T03:00:00Z"},
	})
}

// finishRun gives a Job the Complete or the Failed condition.
func (h *harness) finishRun(name, condition string) {
	h.api.update(jobsResource, "example", name, func(object map[string]any) {
		object["status"].(map[string]any)["conditions"] = []any{map[string]any{
			"type":               condition,
			"status":             "True",
			"reason":             "BackoffLimitExceeded",
			"lastTransitionTime": "2026-09-27T03:05:00Z",
		}}
	})
}

func TestACronJobCheckPingsEachRun(t *testing.T) {
	h := startHarness(t)
	h.api.create(cronJobsResource, backupCronJob())
	h.api.create(checksResource, backupCheck())
	h.pass()
	uuid := h.check("example", "database-backup").Status.UUID

	h.startRun("backup-1")
	h.pass()
	started := h.pingKinds(uuid)
	h.finishRun("backup-1", "Complete")
	h.pass()
	h.startRun("backup-2")
	h.finishRun("backup-2", "Failed")
	h.pass()

	mustMatch(t, started, "start")
	mustMatch(t, h.pingKinds(uuid), "start, success, start, fail: backup-2: BackoffLimitExceeded")
	mustMatch(t, h.check("example", "database-backup").Status.LastReportedJob, "backup-2")
}

// A run that finished while the operator was down gets its pings on
// the first pass after the restart, from where status says the
// reporting stood.
func TestACronJobCheckCatchesUpAfterARestart(t *testing.T) {
	h := startHarness(t)
	h.api.create(cronJobsResource, backupCronJob())
	h.api.create(checksResource, backupCheck())
	h.startRun("backup-1")
	h.finishRun("backup-1", "Complete")
	h.pass()
	uuid := h.check("example", "database-backup").Status.UUID

	h.c = h.restart(settings{namespace: operatorNamespace})
	h.startRun("backup-2")
	h.finishRun("backup-2", "Complete")
	h.startRun("backup-3")
	h.finishRun("backup-3", "Complete")
	h.pass()

	mustMatch(t, h.pingKinds(uuid), "success, start, success, start, success")
	mustMatch(t, h.check("example", "database-backup").Status.LastReportedJob, "backup-3")
}
