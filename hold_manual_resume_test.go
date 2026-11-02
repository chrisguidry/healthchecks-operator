package main

import (
	"testing"
	"time"
)

// manual_resume is a person's setting, "Ignore the ping, stay in the
// paused state" in the check's filtering rules, and the operator leaves
// it alone on every check that a hold did not pause.

func TestARestartedOperatorLeavesAPersonsManualResume(t *testing.T) {
	h := startHarness(t)
	h.upCheck()
	h.hc.SetManualResume("example-website", true)

	h.c = h.restart(settings{namespace: operatorNamespace})
	h.pass()

	stored, _ := h.hc.Check("example-website")
	mustMatch(t, stored.ManualResume, true)
}

func TestAHoldLeavesACheckAPersonPaused(t *testing.T) {
	h := startHarness(t)
	h.upCheck()
	h.hc.SetStatus("example-website", "paused")
	h.hc.SetManualResume("example-website", true)
	h.holdWhileRollingOut("3h")
	h.startRollout()
	h.pass()
	h.runProbes()
	h.pass()

	h.finishRollout()
	h.pass()
	h.runProbes()
	h.pass()

	stored, _ := h.hc.Check("example-website")
	mustMatch(t, stored.Status, "paused")
	mustMatch(t, stored.ManualResume, true)
}

func TestAHoldLeavesManualResumeAloneOnADownCheck(t *testing.T) {
	h := startHarness(t)
	h.upCheck()
	h.hc.SetStatus("example-website", "down")
	h.hc.SetManualResume("example-website", true)
	h.holdWhileRollingOut("3h")
	h.startRollout()

	h.pass()

	stored, _ := h.hc.Check("example-website")
	mustMatch(t, stored.ManualResume, true)
}

func TestAHoldThatEndsAfterARestartClearsManualResume(t *testing.T) {
	h := startHarness(t)
	h.upCheck()
	h.holdWhileRollingOut("3h")
	h.startRollout()
	h.pass()
	h.c = h.restart(settings{namespace: operatorNamespace})
	h.pass()

	h.runProbes()
	h.finishRollout()
	h.pass()

	stored := h.resumed("example-website", "up")
	mustMatch(t, stored.ManualResume, false)
}

// The hold records heldSince with manual_resume, before the pause, so a
// pause that Healthchecks applied though the operator saw it fail still
// ends with the hold: manual_resume false and the check resumed. Without
// heldSince the check would look paused by a person, and ignore every
// ping for good.
func TestAPauseTheOperatorSawFailEndsWithTheHold(t *testing.T) {
	h := startHarness(t)
	h.upCheck()
	h.holdWhileRollingOut("3h")
	h.startRollout()
	pause := "/pause"
	h.refusedSuffix.Store(&pause)
	h.pass()

	refused, _ := h.hc.Check("example-website")
	mustMatch(t, refused.ManualResume, true)
	mustMatch(t, h.check("example", "website").Status.HeldSince != "", true)

	h.hc.SetStatus("example-website", "paused")
	h.refusedSuffix.Store(nil)
	h.clock.advance(time.Minute)
	h.pass()
	h.runProbes()
	h.finishRollout()
	h.pass()

	stored := h.resumed("example-website", "up")
	mustMatch(t, stored.ManualResume, false)
}
