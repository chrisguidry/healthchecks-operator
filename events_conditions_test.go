package main

import (
	"testing"
	"time"
)

// startEvents runs the controller's recorder until the test ends.
func (h *harness) startEvents() {
	go h.c.events.run(h.t.Context(), time.Millisecond, time.Second)
}

// A condition that changes its status or its reason posts one Event,
// with the condition's reason and message, as a Warning when a person
// may need to act.
func TestEachConditionTransitionPostsOneEvent(t *testing.T) {
	h := startHarness(t)
	h.startEvents()
	h.api.create(checksResource, httpCheck("example", "website", "https://example.com"))

	h.pass()
	ready := h.api.waitForEvent(checkKind, "website", reasonSynced)
	h.refuse("POST", `{"error": "refused by the test"}`)
	h.editCheck("example", "website", func(spec map[string]any) { spec["description"] = "Refused" })
	h.pass()
	failed := h.api.waitForEvent(checkKind, "website", reasonUpsertFailed)

	mustMatch(t, ready.Type, eventNormal)
	mustMatch(t, ready.Message, "the check exists in Healthchecks and matches the spec")
	mustMatch(t, failed.Type, eventWarning)
	mustMatch(t, len(h.api.eventsAbout(checkKind, "website")), 2)
}

// A pass that changes no condition posts nothing.
func TestASteadyPassPostsNoEvent(t *testing.T) {
	h := startHarness(t)
	h.startEvents()
	h.api.create(checksResource, httpCheck("example", "website", "https://example.com"))
	h.pass()
	h.api.waitForEvent(checkKind, "website", reasonSynced)

	h.pass()
	h.pass()

	mustMatch(t, len(h.api.eventsAbout(checkKind, "website")), 1)
}

// An Event about a ClusterProject, which has no namespace, goes in the
// namespace default.
func TestAClusterProjectsEventsGoInDefault(t *testing.T) {
	h := startHarness(t)
	h.startEvents()

	h.pass()

	event := h.api.waitForEvent(clusterProjectKind, "internal", reasonConnected)
	mustMatch(t, event.Metadata.Namespace, "default")
}
