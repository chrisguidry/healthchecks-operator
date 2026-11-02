package main

// A project's hold pauses each of its checks, and resumes each one
// when the hold ends. The pause is Healthchecks' own: a paused check
// alerts on no missed ping, and with manual_resume set it records each
// ping as ignored and keeps its status, a failure included
// (hc.api.models.Check.ping).
//
// The hold ends with a ping, not with the resume endpoint. A resume
// makes the check new, and a new check never goes down until a ping
// arrives (hc.api.views.resume), so a workload that stopped during the
// hold would never alert. A success resumes the check with no
// notification, and a failure makes it down, which notifies
// (hc.api.models.Flip.select_channels).
//
// A probe check resumes with its next probe, which the end of the hold
// makes due at once. Its latest result is from inside the hold, often
// from the hold's last minutes, when the thing it watches is still
// coming back. Sent as the resume, that failure would make the check
// down, and the next probe would make it up seconds later, with an
// alert for each. The first probes after the hold can fail the same
// way, because a hold ends when its condition stops matching, and the
// workloads a rollout restarted may still be starting. So a failure
// after the hold logs its reason and keeps the check paused until a
// probe passes, or until the check's interval plus its grace pass
// after the hold. That is the time the check would have had to pass
// with no hold. A cronJob check resumes with its latest Job's result,
// and a ping check with a success.

import (
	"context"
	"time"

	"github.com/chrisguidry/healthchecks-operator/healthchecks"
)

// reasonHoldFailed is the Ready reason of a Check whose pause or
// resume failed. The next pass tries again.
const reasonHoldFailed = "HoldFailed"

// holdCheck pauses the check when its project's hold starts, and
// resumes it when the hold ends. The pass's upsert has already sent
// manual_resume to match the hold, so a pause holds through every
// ping, and the resume's ping counts.
func (c *controller) holdCheck(ctx context.Context, state *checkState, check *Check, grace time.Duration, project *projectState, next *CheckStatus) verdict {
	name := check.Spec.ProjectRef.Name
	if project.holding {
		state.resumeAfter, state.resumeBy = time.Time{}, time.Time{}
		return c.pauseCheck(ctx, state, name, project, next)
	}
	if next.HeldSince == "" {
		return synced
	}
	if check.Spec.ProbeKind().timed() {
		// upsertRequest has already parsed the interval.
		interval, _ := probeInterval(check.Spec)
		c.awaitProbe(state, interval+grace)
		return synced
	}
	kind, body := healthchecks.PingSuccess, "the hold on ClusterProject "+name+" ended"
	if passing, found := findCondition(next.Conditions, passingCondition); found && passing.Status == ConditionFalse {
		kind, body = healthchecks.PingFail, passing.Message
	}
	err := healthchecks.Ping(ctx, c.http, next.PingURL, kind, body)
	if err != nil {
		return verdict{reasonHoldFailed, "resuming the check after the hold: " + errorText(err)}
	}
	c.log.printf("%s: resumed check %s in Healthchecks, because the hold ended", state.subject(), next.Slug)
	next.HeldSince = ""
	return synced
}

// awaitProbe makes the check's probe due at once, the first time a pass
// finds its hold ended. report resumes the check with the first pass
// from a run that started after this, or with the first failure once
// patience has passed. A probe that is not registered yet is due at
// once when the pass registers it. An operator that restarts in the
// wait starts it again, because the wait is held only in memory.
func (c *controller) awaitProbe(state *checkState, patience time.Duration) {
	if !state.resumeAfter.IsZero() {
		return
	}
	state.resumeAfter = c.now()
	state.resumeBy = state.resumeAfter.Add(patience)
	c.schedule.expedite(state.namespace + "/" + state.name)
}

// pauseCheck pauses the check once for each hold. A check that is down
// has notified already, and a check that a person paused stays paused
// after the hold, so the hold pauses neither.
func (c *controller) pauseCheck(ctx context.Context, state *checkState, name string, project *projectState, next *CheckStatus) verdict {
	if next.HeldSince != "" || state.holdSeen == project.since {
		return synced
	}
	if state.upserted == "" || state.upserted == "down" || state.upserted == "paused" {
		state.holdSeen = project.since
		return synced
	}
	if err := project.client.Pause(ctx, next.UUID); err != nil {
		// The next pass sends the upsert again, because its response
		// is the check's status, which the pause needs.
		state.sent = nil
		return verdict{reasonHoldFailed, "pausing the check for the hold: " + errorText(err)}
	}
	c.log.printf("%s: paused check %s in Healthchecks, because ClusterProject %s holds its checks", state.subject(), next.Slug, name)
	state.holdSeen, next.HeldSince = project.since, project.since
	return synced
}
