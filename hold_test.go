package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// rolloutsResource is the object a hold watches in these tests: one
// cluster-scoped Rollout whose Progressing condition says a rollout is
// under way.
var rolloutsResource = kubeResource{Group: "example.com", Version: "v1", Resource: "rollouts"}

// rollout is the Rollout "fleet" with its Progressing condition.
func rollout(status ConditionStatus, reason string) map[string]any {
	return map[string]any{
		"apiVersion": "example.com/v1",
		"kind":       "Rollout",
		"metadata":   map[string]any{"name": "fleet"},
		"status": map[string]any{
			"conditions": []any{map[string]any{
				"type":               "Progressing",
				"status":             string(status),
				"reason":             reason,
				"lastTransitionTime": "2026-09-27T12:00:00Z",
			}},
		},
	}
}

// startRollout creates the Rollout in the middle of a rollout.
func (h *harness) startRollout() {
	h.api.create(rolloutsResource, rollout(ConditionTrue, "RollingOut"))
}

// finishRollout moves the Rollout's condition to a finished rollout.
// Progressing stays True, the way a Deployment's does.
func (h *harness) finishRollout() {
	h.setRollout(ConditionTrue, "RolloutComplete")
}

func (h *harness) setRollout(status ConditionStatus, reason string) {
	h.api.update(rolloutsResource, "", "fleet", func(object map[string]any) {
		object["status"] = rollout(status, reason)["status"]
	})
}

// holdWhileRollingOut gives the project "internal" a hold that lasts
// while the Rollout "fleet" is Progressing for the reason RollingOut.
func (h *harness) holdWhileRollingOut(maxDuration string) {
	h.api.update(clusterProjectsResource, "", "internal", func(object map[string]any) {
		object["spec"].(map[string]any)["hold"] = map[string]any{
			"while": map[string]any{
				"apiVersion": "example.com/v1",
				"resource":   "rollouts",
				"name":       "fleet",
				"condition":  map[string]any{"type": "Progressing", "status": "True", "reason": "RollingOut"},
			},
			"maxDuration": maxDuration,
		}
	})
}

// upCheck is an http Check whose first probe passed, so its check in
// Healthchecks is up.
func (h *harness) upCheck() Check {
	h.t.Helper()
	h.api.create(checksResource, httpCheck("example", "website", startSite(h.t, http.StatusOK)))
	h.runProbes()
	h.pass()
	return h.probedCheck("example", "website")
}

// holding is the project's Holding condition.
func (h *harness) holding() Condition {
	found, _ := findCondition(h.project("internal").Status.Conditions, holdingCondition)
	return found
}

func TestAHoldPausesEachCheckWhileTheConditionMatches(t *testing.T) {
	h := startHarness(t)
	h.upCheck()
	h.holdWhileRollingOut("3h")
	h.startRollout()

	h.pass()

	stored, _ := h.hc.Check("example-website")
	mustMatch(t, stored.Status, "paused")
	mustMatch(t, stored.ManualResume, true)
	mustMatch(t, len(h.hc.Notifications(stored.UUID)), 0)
	mustMatch(t, h.holding().Status, ConditionTrue)
	mustMatch(t, h.holding().Reason, "Matched")
	mustMatch(t, h.holding().LastTransitionTime, "2026-09-27T12:00:00Z")
}

// The probe goes on during a hold, and Healthchecks ignores its pings.
// When the hold ends, one ping carries the latest result: a probe that
// passed resumes the check with no notification, and a probe that
// failed makes it down, which notifies.
func TestAHoldResumesEachCheckWithItsLatestResult(t *testing.T) {
	cases := []struct {
		name          string
		site          int
		status        string
		notifications int
		pingKind      string
	}{
		{"a passing probe", http.StatusOK, "up", 0, "success"},
		{"a failing probe", http.StatusBadGateway, "down", 1, "fail"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := startHarness(t)
			h.holdWhileRollingOut("3h")
			h.startRollout()
			h.api.create(checksResource, httpCheck("example", "website", startSite(t, tc.site)))
			h.runProbes()
			h.pass()
			check := h.probedCheck("example", "website")
			held, _ := h.hc.Check("example-website")
			mustMatch(t, held.Status, "paused")

			h.finishRollout()
			h.pass()

			stored, _ := h.hc.Check("example-website")
			mustMatch(t, stored.Status, tc.status)
			mustMatch(t, stored.ManualResume, false)
			mustMatch(t, len(h.hc.Notifications(stored.UUID)), tc.notifications)
			pings := h.hc.Pings(check.Status.UUID)
			mustMatch(t, pings[0].Kind, "ign")
			mustMatch(t, pings[len(pings)-1].Kind, tc.pingKind)
			mustMatch(t, h.holding().Reason, "NotMatched")
		})
	}
}

// A check that is down when the hold starts has already notified, so
// the hold leaves it down, and its end sends nothing to it.
func TestAHoldLeavesADownCheckDown(t *testing.T) {
	h := startHarness(t)
	check := h.upCheck()
	h.hc.SetStatus("example-website", "down")
	h.holdWhileRollingOut("3h")
	h.startRollout()
	h.pass()
	held, _ := h.hc.Check("example-website")
	mustMatch(t, held.Status, "down")

	h.finishRollout()
	h.pass()

	stored, _ := h.hc.Check("example-website")
	mustMatch(t, stored.Status, "down")
	mustMatch(t, h.pingKinds(check.Status.UUID), "success")
}

// A ping check gets its pings from its workload, so the operator holds
// no result for it, and the end of the hold pings success. The check's
// timer starts again, and a workload that stopped during the hold
// makes it down at its next deadline.
func TestAHoldResumesAPingCheckWithASuccess(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, pingCheck(PingProbe{Timeout: "24h"}))
	h.holdWhileRollingOut("3h")
	h.startRollout()
	h.pass()
	held, _ := h.hc.Check("example-database-backup")
	mustMatch(t, held.Status, "paused")

	h.finishRollout()
	h.pass()

	stored, _ := h.hc.Check("example-database-backup")
	mustMatch(t, stored.Status, "up")
	mustMatch(t, h.pingKinds(stored.UUID), "success: the hold on ClusterProject internal ended")
}

func TestAHoldEndsAtItsMaxDuration(t *testing.T) {
	h := startHarness(t)
	h.upCheck()
	h.holdWhileRollingOut("3h")
	h.startRollout()
	h.pass()

	h.clock.advance(3 * time.Hour)
	h.pass()
	h.pass()

	stored, _ := h.hc.Check("example-website")
	mustMatch(t, stored.Status, "up")
	mustMatch(t, h.holding().Status, ConditionFalse)
	mustMatch(t, h.holding().Reason, "Expired")
}

// An expired hold waits for the condition to stop matching, so the
// next rollout holds the checks again.
func TestTheNextMatchHoldsAgainAfterAHoldExpires(t *testing.T) {
	h := startHarness(t)
	h.upCheck()
	h.holdWhileRollingOut("3h")
	h.startRollout()
	h.pass()
	h.clock.advance(3 * time.Hour)
	h.pass()
	h.finishRollout()
	h.pass()

	h.setRollout(ConditionTrue, "RollingOut")
	h.pass()

	stored, _ := h.hc.Check("example-website")
	mustMatch(t, stored.Status, "paused")
	mustMatch(t, h.holding().Reason, "Matched")
}

// The checks a hold paused are in their Checks' status, so an operator
// that restarts during a hold resumes them when it ends.
func TestARestartedOperatorResumesTheChecksItHeld(t *testing.T) {
	h := startHarness(t)
	h.upCheck()
	h.holdWhileRollingOut("3h")
	h.startRollout()
	h.pass()

	h.c = h.restart(settings{namespace: operatorNamespace})
	h.finishRollout()
	h.pass()

	stored, _ := h.hc.Check("example-website")
	mustMatch(t, stored.Status, "up")
	mustMatch(t, h.check("example", "website").Status.HeldSince, "")
}

func TestRemovingTheHoldResumesTheChecks(t *testing.T) {
	h := startHarness(t)
	h.upCheck()
	h.holdWhileRollingOut("3h")
	h.startRollout()
	h.pass()

	h.api.update(clusterProjectsResource, "", "internal", func(object map[string]any) {
		delete(object["spec"].(map[string]any), "hold")
	})
	h.pass()

	stored, _ := h.hc.Check("example-website")
	mustMatch(t, stored.Status, "up")
	_, found := findCondition(h.project("internal").Status.Conditions, holdingCondition)
	mustMatch(t, found, false)
	mustMatch(t, len(h.c.targets), 0)
}

// A refused list, such as one that RBAC stops, holds nothing, and the
// next pass lists again.
func TestAHoldThatCannotListItsObjectHoldsNothing(t *testing.T) {
	h := startHarness(t)
	h.upCheck()
	h.holdWhileRollingOut("3h")
	h.startRollout()
	h.api.refuse("GET", rolloutsResource.path("", ""), http.StatusForbidden)

	h.pass()

	stored, _ := h.hc.Check("example-website")
	mustMatch(t, stored.Status, "up")
	mustMatch(t, h.holding().Reason, "TargetUnreadable")
	mustMatch(t, strings.Contains(h.holding().Message, "403 Forbidden"), true)

	h.api.allow("GET", rolloutsResource.path("", ""))
	h.pass()

	mustMatch(t, h.holding().Reason, "Matched")
}

// A hold that cannot read its object holds nothing, so a mistake in
// the hold never silences the checks.
func TestAHoldOnAMissingObjectHoldsNothing(t *testing.T) {
	h := startHarness(t)
	h.upCheck()
	h.holdWhileRollingOut("3h")

	h.pass()

	stored, _ := h.hc.Check("example-website")
	mustMatch(t, stored.Status, "up")
	mustMatch(t, h.holding().Status, ConditionFalse)
	mustMatch(t, h.holding().Reason, "TargetNotFound")
}
