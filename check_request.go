package main

// The upsert request is the whole state a Check declares for its check
// in Healthchecks. The operator derives the schedule from the probe,
// so the check and the thing it watches cannot disagree.

import (
	"fmt"
	"time"

	"github.com/chrisguidry/healthchecks-operator/healthchecks"
)

// defaultGrace is the grace period of a Check that states none. It is
// the default of a check made in the Healthchecks web UI.
const defaultGrace = time.Hour

// upsertRequest builds the request for a Check, or the verdict that
// says why it cannot.
func (c *controller) upsertRequest(check *Check, project *projectState, w *world) (healthchecks.UpsertRequest, verdict) {
	request := healthchecks.UpsertRequest{
		Name:        check.EffectiveDisplayName(),
		Slug:        check.EffectiveSlug(),
		Description: check.Spec.Description,
		Tags:        check.Spec.Tags,
		Grace:       defaultGrace,
	}
	if check.Spec.Grace != "" {
		grace, err := time.ParseDuration(check.Spec.Grace)
		if err != nil {
			return request, verdict{reasonInvalidSpec, "grace: " + errorText(err)}
		}
		request.Grace = grace
	}
	channels, v := project.channelIDs(check.Spec.Channels)
	if !v.ready() {
		return request, v
	}
	request.Channels = channels
	request.Period, v = checkPeriod(check, w)
	return request, v
}

// checkPeriod is how often the check expects a ping. A probe's interval
// is the check's timeout, a CronJob's schedule is the check's schedule,
// and a ping check states its own schedule or timeout.
func checkPeriod(check *Check, w *world) (healthchecks.Period, verdict) {
	switch check.Spec.ProbeKind() {
	case ProbeKindPing:
		return pingPeriod(*check.Spec.Ping)
	case ProbeKindCronJob:
		return cronJobPeriod(check, w)
	default:
		interval, err := probeInterval(check.Spec)
		if err != nil {
			return healthchecks.Period{}, verdict{reasonInvalidSpec, errorText(err)}
		}
		return healthchecks.FixedTimeout(interval), synced
	}
}

// cronJobPeriod copies the CronJob's schedule and time zone.
func cronJobPeriod(check *Check, w *world) (healthchecks.Period, verdict) {
	cronJob, found := w.cronJobs[cronJobKey(check)]
	if !found {
		return healthchecks.Period{}, verdict{reasonCronJobNotFound, fmt.Sprintf("CronJob %s does not exist", cronJobKey(check))}
	}
	if problem := historyProblem(cronJob); problem != "" {
		return healthchecks.Period{}, verdict{reasonCronJobHistory, problem}
	}
	return healthchecksPeriod(cronJob), synced
}

// cronJobKey names the CronJob a cronJob check reports, as
// namespace/name. The CronJob is in the Check's own namespace.
func cronJobKey(check *Check) string {
	return check.Metadata.Namespace + "/" + check.Spec.CronJob.Name
}

// probeInterval is how often an http or tls probe runs.
func probeInterval(spec CheckSpec) (time.Duration, error) {
	var interval string
	switch spec.ProbeKind() {
	case ProbeKindHTTP:
		interval = spec.HTTP.Interval
	case ProbeKindTLS:
		interval = spec.TLS.Interval
	default:
		return 0, fmt.Errorf("the Check names no probe")
	}
	parsed, err := time.ParseDuration(interval)
	if err != nil {
		return 0, fmt.Errorf("interval: %w", err)
	}
	return parsed, nil
}
