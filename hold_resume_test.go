package main

// These tests cover how a probe check resumes after a hold: it waits
// for a pass, or for its interval plus its grace, before it goes down.

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// heldFailing is an http Check against a site that answers 502, made
// during a hold so that its check in Healthchecks is paused. The test
// sets the site's answer through the returned value.
func (h *harness) heldFailing() (Check, *atomic.Int32) {
	h.t.Helper()
	h.holdWhileRollingOut("3h")
	h.startRollout()
	url, site := switchSite(h.t, http.StatusBadGateway)
	h.api.create(checksResource, httpCheck("example", "website", url))
	h.runProbes()
	h.pass()
	check := h.probedCheck("example", "website")
	held, _ := h.hc.Check("example-website")
	mustMatch(h.t, held.Status, "paused")
	return check, site
}

// lastPing waits until the check's latest ping has kind.
func (h *harness) lastPing(uuid, kind string) {
	h.t.Helper()
	eventually(h.t, "a "+kind+" ping", func() bool {
		pings := h.hc.Pings(uuid)
		return len(pings) > 0 && pings[len(pings)-1].Kind == kind
	})
}

// The probe goes on during a hold, and Healthchecks ignores its pings.
// When the hold ends, the probe runs again at once, and a pass resumes
// the check with no notification. A failure from inside the hold does
// not count, because the hold exists to ignore those.
func TestAHoldResumesACheckWithAPassFromAfterTheHold(t *testing.T) {
	h := startHarness(t)
	check, site := h.heldFailing()

	site.Store(http.StatusOK)
	h.finishRollout()
	h.pass()

	stored := h.resumed("example-website", "up")
	mustMatch(t, stored.ManualResume, false)
	mustMatch(t, len(h.hc.Notifications(stored.UUID)), 0)
	pings := h.hc.Pings(check.Status.UUID)
	mustMatch(t, pings[0].Kind, "ign")
	mustMatch(t, pings[len(pings)-1].Kind, "success")
	mustMatch(t, h.holding().Reason, "NotMatched")
	eventually(t, "the hold to leave status", func() bool {
		return h.check("example", "website").Status.HeldSince == ""
	})
}

// A thing that a rollout restarted can still be coming back when the
// hold ends. A failure after the hold logs its reason and keeps the
// check paused, until a probe passes or the check's interval plus its
// grace, 5m and 15m here, pass after the hold. Then a failure makes
// the check down, as it would have gone down with no hold.
func TestAHoldWaitsForTheCheckToPassAfterItEnds(t *testing.T) {
	cases := []struct {
		name          string
		after         time.Duration
		answer        int
		status        string
		notifications int
		pingKind      string
	}{
		{"a target that recovers within the grace", 10 * time.Minute, http.StatusOK, "up", 0, "success"},
		{"a target that fails within the grace", 10 * time.Minute, http.StatusBadGateway, "paused", 0, "log"},
		{"a target that recovers after the grace", 20 * time.Minute, http.StatusOK, "up", 0, "success"},
		{"a target that fails after the grace", 20 * time.Minute, http.StatusBadGateway, "down", 1, "fail"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := startHarness(t)
			check, site := h.heldFailing()
			h.finishRollout()
			h.pass()
			h.lastPing(check.Status.UUID, "log")
			stored, _ := h.hc.Check("example-website")
			mustMatch(t, stored.Status, "paused")

			h.clock.advance(tc.after)
			site.Store(int32(tc.answer))
			pings := len(h.hc.Pings(check.Status.UUID))
			h.c.schedule.expedite("example/website")
			eventually(t, "the next probe's ping", func() bool {
				return len(h.hc.Pings(check.Status.UUID)) > pings
			})

			stored, _ = h.hc.Check("example-website")
			mustMatch(t, stored.Status, tc.status)
			mustMatch(t, len(h.hc.Notifications(stored.UUID)), tc.notifications)
			mustMatch(t, h.hc.Pings(check.Status.UUID)[pings].Kind, tc.pingKind)
		})
	}
}

// A hold that starts while a check waits for its first pass keeps the
// check paused, and the wait starts again when that hold ends.
func TestAHoldDuringTheWaitStartsTheWaitAgain(t *testing.T) {
	h := startHarness(t)
	check, _ := h.heldFailing()
	h.finishRollout()
	h.pass()
	h.lastPing(check.Status.UUID, "log")

	h.clock.advance(10 * time.Minute)
	h.setRollout(ConditionTrue, "RollingOut")
	h.pass()
	h.finishRollout()
	h.pass()
	h.clock.advance(15 * time.Minute)
	pings := len(h.hc.Pings(check.Status.UUID))
	h.c.schedule.expedite("example/website")
	eventually(t, "the next probe's ping", func() bool {
		return len(h.hc.Pings(check.Status.UUID)) > pings
	})

	stored, _ := h.hc.Check("example-website")
	mustMatch(t, stored.Status, "paused")
	mustMatch(t, h.hc.Pings(check.Status.UUID)[pings].Kind, "log")
	mustMatch(t, h.check("example", "website").Status.HeldSince != "", true)
}
