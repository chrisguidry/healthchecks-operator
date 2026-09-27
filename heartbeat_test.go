package main

import (
	"strings"
	"testing"
	"time"
)

// startHeartbeatHarness is a harness whose controller pings the
// heartbeat check operator-heartbeat in project.
func startHeartbeatHarness(t *testing.T, project string) *harness {
	t.Helper()
	h := startHarness(t)
	h.c = h.restart(settings{namespace: operatorNamespace, heartbeatProject: project, heartbeatSlug: "operator-heartbeat"})
	return h
}

func TestTheHeartbeatCheckIsCreatedOnceItsProjectIsReady(t *testing.T) {
	h := startHeartbeatHarness(t, "internal")

	h.pass()
	h.pass()

	stored, found := h.hc.Check("operator-heartbeat")
	mustMatch(t, found, true)
	mustMatch(t, stored.Name, "healthchecks-operator")
	mustMatch(t, stored.Timeout, time.Minute)
	mustMatch(t, stored.Grace, time.Minute)
	mustMatch(t, strings.Join(stored.Channels, ","), h.pushover)
	mustMatch(t, strings.Join(h.log.lines(), "\n"), "heartbeat operator-heartbeat: created check operator-heartbeat in Healthchecks")
}

func TestTheHeartbeatWaitsForItsProject(t *testing.T) {
	h := startHeartbeatHarness(t, "outside")

	h.pass()

	_, found := h.hc.Check("operator-heartbeat")
	mustMatch(t, found, false)
	mustMatch(t, strings.Join(h.log.lines(), "\n"), "heartbeat operator-heartbeat: ClusterProject outside is not ready, so the operator sends no heartbeat")
}

// A loop that finished no pass in two minutes is stuck, and a stuck
// loop stops the heartbeat, so the heartbeat check goes down.
func TestTheHeartbeatStopsWhenPassesStop(t *testing.T) {
	h := startHeartbeatHarness(t, "internal")
	h.pass()
	stored, _ := h.hc.Check("operator-heartbeat")

	h.clock.advance(time.Minute)
	h.c.heartbeat.beat(t.Context())
	h.clock.advance(time.Minute)
	h.c.heartbeat.beat(t.Context())
	h.clock.advance(time.Minute)
	h.c.heartbeat.beat(t.Context())

	mustMatch(t, h.pingKinds(stored.UUID), "success, success")
}

func TestTheHeartbeatBeatsOnItsInterval(t *testing.T) {
	h := startHeartbeatHarness(t, "internal")
	h.pass()
	stored, _ := h.hc.Check("operator-heartbeat")

	go h.c.heartbeat.run(t.Context(), time.Millisecond)

	eventually(t, "a heartbeat ping", func() bool { return len(h.hc.Pings(stored.UUID)) > 0 })
}
