package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chrisguidry/healthchecks-operator/healthchecks/healthcheckstest"
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

// switchSite serves one page that answers with the status the test
// stores in it.
func switchSite(t *testing.T, status int) (string, *atomic.Int32) {
	t.Helper()
	answer := &atomic.Int32{}
	answer.Store(int32(status))
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(answer.Load()))
	}))
	t.Cleanup(site.Close)
	return site.URL + "/", answer
}

// stalledSite answers its first request with a 502 only when the test
// releases it, and every later request with a 200. arrived receives
// once the first request is in flight.
func stalledSite(t *testing.T) (url string, arrived <-chan struct{}, release chan<- struct{}) {
	t.Helper()
	in, out := make(chan struct{}, 1), make(chan struct{})
	var requests atomic.Int32
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) > 1 {
			w.WriteHeader(http.StatusOK)
			return
		}
		in <- struct{}{}
		<-out
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(site.Close)
	return site.URL + "/", in, out
}

// resumed waits until the check in Healthchecks has status, and
// returns it.
func (h *harness) resumed(slug, status string) healthcheckstest.StoredCheck {
	h.t.Helper()
	eventually(h.t, "check "+slug+" to be "+status, func() bool {
		stored, _ := h.hc.Check(slug)
		return stored.Status == status
	})
	stored, _ := h.hc.Check(slug)
	return stored
}

// The probe goes on during a hold, and Healthchecks ignores its pings.
// When the hold ends, the probe runs again at once, and its ping
// resumes the check: a pass resumes it with no notification, and a
// failure makes it down, which notifies. A failure from inside the
// hold does not count, because the hold exists to ignore those.
func TestAHoldResumesEachCheckWithAProbeFromAfterTheHold(t *testing.T) {
	cases := []struct {
		name          string
		after         int
		status        string
		notifications int
		pingKind      string
	}{
		{"a target that recovered", http.StatusOK, "up", 0, "success"},
		{"a target that is still failing", http.StatusBadGateway, "down", 1, "fail"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := startHarness(t)
			h.holdWhileRollingOut("3h")
			h.startRollout()
			url, site := switchSite(t, http.StatusBadGateway)
			h.api.create(checksResource, httpCheck("example", "website", url))
			h.runProbes()
			h.pass()
			check := h.probedCheck("example", "website")
			held, _ := h.hc.Check("example-website")
			mustMatch(t, held.Status, "paused")

			site.Store(int32(tc.after))
			h.finishRollout()
			h.pass()

			stored := h.resumed("example-website", tc.status)
			mustMatch(t, stored.ManualResume, false)
			mustMatch(t, len(h.hc.Notifications(stored.UUID)), tc.notifications)
			pings := h.hc.Pings(check.Status.UUID)
			mustMatch(t, pings[0].Kind, "ign")
			mustMatch(t, pings[len(pings)-1].Kind, tc.pingKind)
			mustMatch(t, h.holding().Reason, "NotMatched")
			eventually(t, "the hold to leave status", func() bool {
				return h.check("example", "website").Status.HeldSince == ""
			})
		})
	}
}

// A probe that started inside the hold and finishes after it reports
// nothing. The probe runs again, and its result resumes the check.
func TestAHoldDropsAProbeThatStartedBeforeItEnded(t *testing.T) {
	h := startHarness(t)
	h.holdWhileRollingOut("3h")
	h.startRollout()
	url, arrived, release := stalledSite(t)
	h.api.create(checksResource, httpCheck("example", "website", url))
	h.runProbes()
	h.pass()
	receive(t, arrived)

	h.clock.advance(time.Minute)
	h.finishRollout()
	h.pass()
	release <- struct{}{}

	stored := h.resumed("example-website", "up")
	mustMatch(t, len(h.hc.Notifications(stored.UUID)), 0)
	mustMatch(t, h.pingKinds(stored.UUID), "success")
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

	h.resumed("example-website", "up")
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
	h.runProbes()
	h.finishRollout()
	h.pass()

	h.resumed("example-website", "up")
	eventually(t, "the hold to leave status", func() bool {
		return h.check("example", "website").Status.HeldSince == ""
	})
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

	h.resumed("example-website", "up")
	_, found := findCondition(h.project("internal").Status.Conditions, holdingCondition)
	mustMatch(t, found, false)
	mustMatch(t, len(h.c.targets), 0)
}

// A pause that fails sets Ready to False, and a pass after the backoff
// pauses the check.
func TestAHoldPausesACheckAgainAfterAFailedPause(t *testing.T) {
	h := startHarness(t)
	h.upCheck()
	h.holdWhileRollingOut("3h")
	h.startRollout()
	pause := "/pause"
	h.refusedSuffix.Store(&pause)

	h.pass()

	mustMatch(t, conditionOf(h.check("example", "website"), "Ready").Reason, "HoldFailed")
	h.refusedSuffix.Store(nil)
	h.clock.advance(time.Minute)
	h.pass()
	stored, _ := h.hc.Check("example-website")
	mustMatch(t, stored.Status, "paused")
	mustMatch(t, conditionOf(h.check("example", "website"), "Ready").Reason, "Synced")
}

func TestAHoldSaysWhyItHoldsNothing(t *testing.T) {
	cases := []struct {
		name    string
		change  func(hold, while map[string]any)
		reason  string
		message string
	}{
		{
			"a namespaced object that does not exist",
			func(hold, while map[string]any) {
				while["apiVersion"], while["resource"], while["namespace"] = "v1", "configmaps", "example"
			},
			"TargetNotFound",
			"configmaps example/fleet does not exist",
		},
		{
			"a maxDuration that does not parse",
			func(hold, while map[string]any) { hold["maxDuration"] = "soon" },
			"InvalidHold",
			`maxDuration: time: invalid duration "soon"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := startHarness(t)
			h.startRollout()
			h.holdWhileRollingOut("3h")
			h.api.update(clusterProjectsResource, "", "internal", func(object map[string]any) {
				hold := object["spec"].(map[string]any)["hold"].(map[string]any)
				tc.change(hold, hold["while"].(map[string]any))
			})

			h.pass()

			mustMatch(t, h.holding().Reason, tc.reason)
			mustMatch(t, h.holding().Message, tc.message)
		})
	}
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
