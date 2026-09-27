package main

import (
	"sort"
	"strings"
	"time"

	"github.com/chrisguidry/healthchecks-operator/healthchecks"
)

// batchCronJob holds the fields of a batch/v1 CronJob that the operator reads.
type batchCronJob struct {
	Metadata ObjectMeta       `json:"metadata"`
	Spec     batchCronJobSpec `json:"spec"`
}

type batchCronJobSpec struct {
	Schedule                   string  `json:"schedule"`
	TimeZone                   *string `json:"timeZone,omitempty"`
	SuccessfulJobsHistoryLimit *int32  `json:"successfulJobsHistoryLimit,omitempty"`
	FailedJobsHistoryLimit     *int32  `json:"failedJobsHistoryLimit,omitempty"`
}

// batchJob holds the fields of a batch/v1 Job that the operator reads.
// ObjectMeta carries name, namespace, and uid; jobMetadata adds the
// creation time and owner references that no other Check kind needs,
// so ObjectMeta itself stays untouched.
type batchJob struct {
	Metadata jobMetadata    `json:"metadata"`
	Status   batchJobStatus `json:"status"`
}

type jobMetadata struct {
	ObjectMeta
	CreationTimestamp time.Time        `json:"creationTimestamp"`
	OwnerReferences   []ownerReference `json:"ownerReferences,omitempty"`
}

// ownerReference is one entry in a Job's ownerReferences. The uid tells
// a Job apart from a same-named Job left by a deleted and recreated
// CronJob. Kind, Name, and Controller are not read yet; they are here
// so a later change can tell the CronJob that controls a Job from
// another reference on it, without a second look at the wire format.
type ownerReference struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller *bool  `json:"controller,omitempty"`
}

type batchJobStatus struct {
	StartTime  *time.Time     `json:"startTime,omitempty"`
	Conditions []jobCondition `json:"conditions,omitempty"`
}

// jobCondition is one entry of a Job's status.conditions. A Job is
// finished once it carries a Complete or a Failed condition with
// Status "True".
type jobCondition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason,omitempty"`
	Message            string    `json:"message,omitempty"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

// ownedBy reports whether uid names one of the Job's owners. The
// operator watches Jobs in every namespace, so this is what tells one
// CronJob's Jobs apart from every other Job the watch delivers.
func (j batchJob) ownedBy(uid string) bool {
	if uid == "" {
		return false
	}
	for _, ref := range j.Metadata.OwnerReferences {
		if ref.UID == uid {
			return true
		}
	}
	return false
}

// trueCondition returns the named condition if it is set to "True".
func (j batchJob) trueCondition(conditionType string) (jobCondition, bool) {
	for _, c := range j.Status.Conditions {
		if c.Type == conditionType && c.Status == "True" {
			return c, true
		}
	}
	return jobCondition{}, false
}

// outcome reports whether the Job is finished, and if so, which ping
// it earns and what the ping's body says. A Failed condition wins over
// a Complete one, though Kubernetes never sets both at once.
func (j batchJob) outcome() (finished bool, kind healthchecks.PingKind, body string) {
	if cond, ok := j.trueCondition("Failed"); ok {
		return true, healthchecks.PingFail, failureBody(j, cond)
	}
	if _, ok := j.trueCondition("Complete"); ok {
		return true, healthchecks.PingSuccess, ""
	}
	return false, healthchecks.PingSuccess, ""
}

// failureBody names the Job and repeats its Failed condition, so the
// check's log in Healthchecks says which run failed and why without a
// trip back to Kubernetes.
func failureBody(j batchJob, failed jobCondition) string {
	body := j.Metadata.Name
	if failed.Reason != "" {
		body += ": " + failed.Reason
	}
	if failed.Message != "" {
		body += ": " + failed.Message
	}
	return body
}

// cronMacros translates the schedule macros cronsim rejects into the
// five-field form it accepts, following the table in plans/00-design.md.
var cronMacros = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

// translateSchedule turns a CronJob schedule into one cronsim accepts.
// A macro from cronMacros expands in full. Otherwise, a "?" in the
// day-of-month or day-of-week field of a five-field schedule becomes
// "*", and everything else passes through unchanged: Healthchecks
// rejects what it rejects, and the reconciler reports its message.
//
// Kubernetes rejects a schedule that starts with a CRON_TZ= or TZ=
// prefix, so a CronJob never carries one. A schedule like that has
// more than five fields, which the field count check below already
// passes through unchanged, so it costs no special case.
func translateSchedule(schedule string) string {
	if translated, ok := cronMacros[schedule]; ok {
		return translated
	}
	fields := strings.Fields(schedule)
	if len(fields) != 5 {
		return schedule
	}
	const dayOfMonth, dayOfWeek = 2, 4
	fields[dayOfMonth] = strings.ReplaceAll(fields[dayOfMonth], "?", "*")
	fields[dayOfWeek] = strings.ReplaceAll(fields[dayOfWeek], "?", "*")
	return strings.Join(fields, " ")
}

// healthchecksPeriod translates a CronJob's schedule and time zone into
// the Healthchecks schedule, following the table in plans/00-design.md.
// An absent time zone is UTC, which is what the CronJob controller uses.
func healthchecksPeriod(c batchCronJob) healthchecks.Period {
	tz := "UTC"
	if c.Spec.TimeZone != nil && *c.Spec.TimeZone != "" {
		tz = *c.Spec.TimeZone
	}
	return healthchecks.CronSchedule(translateSchedule(c.Spec.Schedule), tz)
}

// historyProblem returns why the CronJob's history limits stop the
// operator from reporting runs it missed, or "" when they are fine.
// Kubernetes defaults each limit to a value of 1 or more when the
// CronJob does not set it, so only an explicit 0 is a problem.
func historyProblem(c batchCronJob) string {
	if lim := c.Spec.SuccessfulJobsHistoryLimit; lim != nil && *lim == 0 {
		return "successfulJobsHistoryLimit is 0: the operator needs one finished Job kept to find a run that finished while it was down"
	}
	if lim := c.Spec.FailedJobsHistoryLimit; lim != nil && *lim == 0 {
		return "failedJobsHistoryLimit is 0: the operator needs one finished Job kept to find a run that finished while it was down"
	}
	return ""
}

// jobPing is one ping the reconciler sends for one Job.
type jobPing struct {
	job  string
	kind healthchecks.PingKind
	// body is "" for a start or a success, and says which Job failed and
	// why for a failure.
	body string
}

// cronJobRuns is what the reconciler tracks for one cronJob Check
// between passes. lastReportedJob is persisted in status. lastStartedJob
// is held in memory only: after a restart, a running Job may get a
// second /start, which Healthchecks treats as a new start time.
type cronJobRuns struct {
	lastStartedJob  string
	lastReportedJob string
}

// ownedJobs returns c's own Jobs from jobs, ordered by creation time
// and then by name so two Jobs made in the same instant still sort the
// same way on every call.
func ownedJobs(c batchCronJob, jobs []batchJob) []batchJob {
	var owned []batchJob
	for _, j := range jobs {
		if j.ownedBy(c.Metadata.UID) {
			owned = append(owned, j)
		}
	}
	sort.Slice(owned, func(i, k int) bool {
		ti, tk := owned[i].Metadata.CreationTimestamp, owned[k].Metadata.CreationTimestamp
		if !ti.Equal(tk) {
			return ti.Before(tk)
		}
		return owned[i].Metadata.Name < owned[k].Metadata.Name
	})
	return owned
}

// indexOfJob returns name's position in owned, or -1 when name is ""
// or names a Job that is not there. The two cases are indistinguishable
// from the caller's own state, and both mean "nothing to skip yet."
func indexOfJob(owned []batchJob, name string) int {
	if name == "" {
		return -1
	}
	for i, j := range owned {
		if j.Metadata.Name == name {
			return i
		}
	}
	return -1
}

// reportPlan finds where to resume reporting finished Jobs.
//
// While lastReportedJob still names a Job in owned, reporting resumes
// right after it: the ordinary steady-state case.
//
// Otherwise, lastReportedJob is "" for a Check the operator has never
// reported for, or it names a Job that Kubernetes has since pruned past
// the CronJob's history limit. Either way, cronJobRuns stores only a
// name, never a timestamp, so there is nothing left to place the old
// position by. The safe catch-up is the newest finished Job alone: that
// is the one run whose outcome the Job list can still prove, and it
// pairs with suppressing start pings for every already-finished Job, so
// none of them is left with a start ping and no finish ping to match it.
func reportPlan(owned []batchJob) (reportIndex int, suppressFinishedStarts bool) {
	newest := -1
	for i, j := range owned {
		if finished, _, _ := j.outcome(); finished {
			newest = i
		}
	}
	return newest - 1, true
}

// pendingPings returns the pings to send for the Jobs owned by c, in
// order, and the runs to track once they are sent. jobs may hold Jobs
// of any CronJob; only those whose owner reference is c's uid count.
// When runs.lastReportedJob is empty, only the newest finished Job is
// reported, so a new Check does not replay history.
func pendingPings(c batchCronJob, jobs []batchJob, runs cronJobRuns) ([]jobPing, cronJobRuns) {
	owned := ownedJobs(c, jobs)

	startIndex := indexOfJob(owned, runs.lastStartedJob)
	reportIndex, suppressFinishedStarts := -1, false
	if idx := indexOfJob(owned, runs.lastReportedJob); runs.lastReportedJob != "" && idx >= 0 {
		reportIndex = idx
	} else {
		reportIndex, suppressFinishedStarts = reportPlan(owned)
	}

	next := runs
	var pings []jobPing

	for i, j := range owned {
		finished, kind, body := j.outcome()

		// i > reportIndex as well: a Job at or before it was already
		// reported in an earlier pass, so it was already started then
		// too, even if this pass's lastStartedJob (memory-only) forgets
		// that because the operator restarted since.
		if i > startIndex && i > reportIndex && j.Status.StartTime != nil && !(suppressFinishedStarts && finished) {
			pings = append(pings, jobPing{job: j.Metadata.Name, kind: healthchecks.PingStart})
			next.lastStartedJob = j.Metadata.Name
		}

		if i > reportIndex && finished {
			pings = append(pings, jobPing{job: j.Metadata.Name, kind: kind, body: body})
			next.lastReportedJob = j.Metadata.Name
		}
	}

	return pings, next
}
