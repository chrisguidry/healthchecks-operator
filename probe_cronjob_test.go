package main

// healthchecksPeriod tests. Fixtures shared with the other
// probe_cronjob_*_test.go files live here, since this is the first file
// in the group.

import (
	"testing"
	"time"

	"github.com/chrisguidry/healthchecks-operator/healthchecks"
)

// strp and int32p build the pointers a CronJob spec's optional fields
// need, since Go has no address-of operator for a literal.
func strp(s string) *string { return &s }
func int32p(n int32) *int32 { return &n }

func cronJobWithSchedule(schedule string, timeZone *string) batchCronJob {
	return batchCronJob{Spec: batchCronJobSpec{Schedule: schedule, TimeZone: timeZone}}
}

func TestHealthchecksPeriodDefaultsToUTC(t *testing.T) {
	got := healthchecksPeriod(cronJobWithSchedule("0 3 * * *", nil))
	want := healthchecks.CronSchedule("0 3 * * *", "UTC")
	mustMatch(t, got, want)
}

func TestHealthchecksPeriodUsesTimeZone(t *testing.T) {
	got := healthchecksPeriod(cronJobWithSchedule("0 3 * * *", strp("America/New_York")))
	want := healthchecks.CronSchedule("0 3 * * *", "America/New_York")
	mustMatch(t, got, want)
}

func TestHealthchecksPeriodTranslatesMacros(t *testing.T) {
	cases := []struct {
		name     string
		schedule string
		want     string
	}{
		{"yearly", "@yearly", "0 0 1 1 *"},
		{"annually", "@annually", "0 0 1 1 *"},
		{"monthly", "@monthly", "0 0 1 * *"},
		{"weekly", "@weekly", "0 0 * * 0"},
		{"daily", "@daily", "0 0 * * *"},
		{"midnight", "@midnight", "0 0 * * *"},
		{"hourly", "@hourly", "0 * * * *"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := healthchecksPeriod(cronJobWithSchedule(c.schedule, nil))
			want := healthchecks.CronSchedule(c.want, "UTC")
			mustMatch(t, got, want)
		})
	}
}

func TestHealthchecksPeriodTranslatesQuestionMark(t *testing.T) {
	cases := []struct {
		name     string
		schedule string
		want     string
	}{
		{"day of month", "0 3 ? * *", "0 3 * * *"},
		{"day of week", "0 3 * * ?", "0 3 * * *"},
		{"both", "0 3 ? * ?", "0 3 * * *"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := healthchecksPeriod(cronJobWithSchedule(c.schedule, nil))
			want := healthchecks.CronSchedule(c.want, "UTC")
			mustMatch(t, got, want)
		})
	}
}

func TestHealthchecksPeriodLeavesUnknownSchedulesAlone(t *testing.T) {
	// cronsim rejects this, and Healthchecks reports why; the operator's
	// job is to pass the schedule through, not to validate it.
	got := healthchecksPeriod(cronJobWithSchedule("not a schedule", nil))
	want := healthchecks.CronSchedule("not a schedule", "UTC")
	mustMatch(t, got, want)
}

func TestHealthchecksPeriodDoesNotCrashOnATimeZonePrefix(t *testing.T) {
	// Kubernetes rejects a schedule that starts with CRON_TZ= or TZ=, so
	// a real CronJob never carries one. This only proves the translator
	// does not panic if it ever sees one anyway.
	cases := []string{"CRON_TZ=UTC 0 3 * * *", "TZ=UTC 0 3 * * *"}
	for _, schedule := range cases {
		t.Run(schedule, func(t *testing.T) {
			got := healthchecksPeriod(cronJobWithSchedule(schedule, nil))
			want := healthchecks.CronSchedule(schedule, "UTC")
			mustMatch(t, got, want)
		})
	}
}

// job builds a Job owned by the CronJob at ownerUID, created at
// created. The tests that need a start time or a finished condition
// chain started, succeeded, or failed onto the result.
func job(name, ownerUID string, created time.Time) batchJob {
	return batchJob{
		Metadata: jobMetadata{
			ObjectMeta:        ObjectMeta{Name: name},
			CreationTimestamp: created,
			OwnerReferences:   []ownerReference{{Kind: "CronJob", Name: "backup", UID: ownerUID}},
		},
	}
}

func (j batchJob) started(at time.Time) batchJob {
	j.Status.StartTime = &at
	return j
}

func (j batchJob) succeeded(at time.Time) batchJob {
	j.Status.Conditions = append(j.Status.Conditions, jobCondition{
		Type: "Complete", Status: "True", LastTransitionTime: at,
	})
	return j
}

func (j batchJob) failed(reason, message string, at time.Time) batchJob {
	j.Status.Conditions = append(j.Status.Conditions, jobCondition{
		Type: "Failed", Status: "True", Reason: reason, Message: message, LastTransitionTime: at,
	})
	return j
}

// at returns a fixed time offset by minute, so a test can order Jobs by
// small, readable numbers instead of full timestamps.
func at(minute int) time.Time {
	return time.Date(2026, 1, 1, 0, minute, 0, 0, time.UTC)
}

// mustMatchPings compares two ping slices in order. []jobPing is not
// comparable, so it cannot use the package's generic mustMatch.
func mustMatchPings(t *testing.T, got, want []jobPing) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d pings %+v, want %d pings %+v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ping %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}
