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
// resumes it when the hold ends. At the end, the pass's upsert has
// already sent manual_resume false, so the resume's ping counts.
func (c *controller) holdCheck(ctx context.Context, state *checkState, check *Check, request healthchecks.UpsertRequest, project *projectState, next *CheckStatus) verdict {
	name := check.Spec.ProjectRef.Name
	if project.holding {
		state.resumeAfter, state.resumeBy = time.Time{}, time.Time{}
		return c.pauseCheck(ctx, state, name, project, request, next)
	}
	if next.HeldSince == "" {
		return synced
	}
	if check.Spec.ProbeKind().timed() {
		// upsertRequest has already parsed the interval.
		interval, _ := probeInterval(check.Spec)
		c.awaitProbe(state, interval+request.Grace)
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
	state.released()
	return synced
}

// released records that the hold no longer sets the check's
// manual_resume. Healthchecks keeps the false that the end of the hold
// sent, and the requests that follow leave the field alone, so the
// request sent last stands for them. Without this, the next pass sends
// the same request again only to omit the field.
func (s *checkState) released() {
	if s.sent == nil || s.sent.ManualResume == nil {
		return
	}
	sent := *s.sent
	sent.ManualResume = nil
	s.sent = &sent
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
// has notified already, and a check that a person paused is theirs to
// resume, so the hold pauses neither and leaves their manual_resume
// alone.
//
// The pause sets manual_resume first, so a ping between the two calls
// cannot resume the check. That replaces the setting a person gave the
// check, and the end of the hold sets it to false, which is
// Healthchecks' default. The setting matters only while a check is
// paused, and the hold pauses only a check that is not.
//
// status.heldSince is set with manual_resume, before the pause, because
// only a check with heldSince gets manual_resume false when the hold
// ends. A check paused with manual_resume and no heldSince would ignore
// every ping for good. So a check with heldSince whose pause is not
// confirmed, after a failed pause or a restart, is paused again unless
// Healthchecks already has it paused.
func (c *controller) pauseCheck(ctx context.Context, state *checkState, name string, project *projectState, request healthchecks.UpsertRequest, next *CheckStatus) verdict {
	switch {
	case state.holdPaused == project.since:
		// A status write that failed lost heldSince, so it goes in again.
		if next.HeldSince == "" {
			next.HeldSince = project.since
		}
		return synced
	case next.HeldSince == "" && state.holdSkipped == project.since:
		return synced
	}
	status := state.upserted
	if status == "" {
		// The pass sent no upsert, because the request did not change.
		// The same request reads the check's status and changes nothing.
		result, err := project.client.Upsert(ctx, request)
		if err != nil {
			return verdict{reasonHoldFailed, "reading the check's status for the hold: " + errorText(err)}
		}
		status = result.Status
	}
	if next.HeldSince != "" && status == "paused" {
		state.holdPaused = project.since
		return synced
	}
	if next.HeldSince == "" {
		if status == "down" || status == "paused" {
			state.holdSkipped = project.since
			return synced
		}
		ignore := true
		request.ManualResume = &ignore
		if _, err := project.client.Upsert(ctx, request); err != nil {
			state.sent = nil
			return verdict{reasonHoldFailed, "ignoring pings for the hold: " + errorText(err)}
		}
		state.sent = &request
		next.HeldSince = project.since
	}
	if err := project.client.Pause(ctx, next.UUID); err != nil {
		// The next pass sends the upsert again, because its response
		// is the check's status, which the pause needs.
		state.sent = nil
		return verdict{reasonHoldFailed, "pausing the check for the hold: " + errorText(err)}
	}
	c.log.printf("%s: paused check %s in Healthchecks, because ClusterProject %s holds its checks", state.subject(), next.Slug, name)
	state.holdPaused = project.since
	return synced
}
