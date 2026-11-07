package main

import (
	"net/http"
	"testing"
)

// A hold's watch selects the one object the hold names, by name.
func TestAHoldWatchesOnlyTheObjectItNames(t *testing.T) {
	h := startHarness(t)
	h.startRollout()
	h.api.create(rolloutsResource, map[string]any{"apiVersion": "example.com/v1", "kind": "Rollout", "metadata": map[string]any{"name": "other"}})
	h.holdWhileRollingOut("3h")

	h.pass()

	mustMatch(t, h.holding().Reason, "Matched")
	mustMatchMap(t, storedVersions(t, h.c.targets[holdTarget().key()].watch.store), map[string]string{"/fleet": h.api.versions(rolloutsResource, "")["/fleet"]})
	for _, request := range h.api.requests() {
		if request.Path == rolloutsResource.path("", "") {
			mustMatch(t, request.Query.Get("fieldSelector"), "metadata.name=fleet")
		}
	}
}

func holdTarget() HoldTarget {
	return HoldTarget{APIVersion: "example.com/v1", Resource: "rollouts", Name: "fleet"}
}

// A restarted operator whose watch has not read the object yet leaves
// a standing hold as it was: the checks stay paused, and the hold keeps
// the time it started.
func TestARestartKeepsAHoldWhileItsObjectIsUnread(t *testing.T) {
	h := startHarness(t)
	h.upCheck()
	h.holdWhileRollingOut("3h")
	h.startRollout()
	h.pass()
	started := h.holding()

	h.c = h.restart(settings{namespace: operatorNamespace})
	h.api.refuse("GET", rolloutsResource.path("", ""), http.StatusForbidden)
	h.pass()

	mustMatch(t, h.holding().Status, ConditionTrue)
	mustMatch(t, h.holding().LastTransitionTime, started.LastTransitionTime)
	stored, _ := h.hc.Check("example-website")
	mustMatch(t, stored.Status, "paused")
}
