package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// runController runs the controller's loop until the test ends, and
// fails the test if the loop does not stop.
func runController(t *testing.T, c *controller) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan error, 1)
	go func() { stopped <- c.run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-stopped:
			mustSucceed(t, err)
		case <-time.After(testTimeout):
			t.Error("the loop did not stop")
		}
	})
}

// A watch event wakes the loop, so a new Check reaches Healthchecks
// without a wait for the backstop tick.
func TestTheLoopReconcilesACheckWhenItAppears(t *testing.T) {
	h := startHarness(t)
	runController(t, h.c)

	h.api.create(checksResource, httpCheck("example", "website", "https://example.com/"))

	eventually(t, "the check in Healthchecks", func() bool {
		_, found := h.hc.Check("example-website")
		return found
	})
}

// The backstop tick runs a pass when nothing changes, which is what
// keeps the heartbeat going.
func TestTheBackstopTickRunsAPass(t *testing.T) {
	h := startHarness(t)
	h.c.backstop = time.Millisecond
	runController(t, h.c)

	// Each pass lists the Checks once, and nothing changes to wake a
	// watch.
	eventually(t, "three passes", func() bool {
		return requestCount(h.api, "/apis/healthchecks.guid.foo/v1alpha1/checks", "") > 3
	})
}

func TestTheLoopStopsWhenTheFirstListFails(t *testing.T) {
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(refusing.Close)
	client := newKubeClient(refusing.URL, refusing.Client(), "")
	c := newController(settings{namespace: operatorNamespace}, client, newMetrics("dev"), time.Now, &syncBuffer{})

	err := c.run(t.Context())

	mustMatch(t, err.Error(), "listing clusterprojects.healthchecks.guid.foo: GET /apis/healthchecks.guid.foo/v1alpha1/clusterprojects: 403 Forbidden: forbidden")
}

// A pass that cannot read the cluster writes one line, and reconciles
// nothing.
func TestAPassThatCannotReadTheClusterSaysSo(t *testing.T) {
	h := startHarness(t)
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(refusing.Close)
	h.c.client = newKubeClient(refusing.URL, refusing.Client(), "")

	h.pass()
	h.pass()

	mustMatch(t, strings.Join(h.log.lines(), "\n"), "reading the cluster: listing clusterprojects.healthchecks.guid.foo: GET /apis/healthchecks.guid.foo/v1alpha1/clusterprojects: 403 Forbidden: forbidden")
}

func TestOperateRunsUntilItsContextEnds(t *testing.T) {
	api := startFakeKube(t)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	err := operate(ctx, settings{namespace: operatorNamespace}, api.client, newMetrics("dev"))

	mustSucceed(t, err)
}

func TestAPassRecordsItsMetrics(t *testing.T) {
	h := startHarness(t)
	check := httpCheck("example", "website", "https://example.com/")
	check.Spec.ProjectRef.Name = "outside"
	h.api.create(checksResource, check)

	h.pass()

	samples := scrape(t, h.c.readings)
	mustMatch(t, slices.Contains(samples, "healthchecks_reconcile_duration_seconds_count 1"), true)
	mustMatch(t, slices.Contains(samples, `healthchecks_reconcile_errors_total{kind="Check"} 1`), true)
}
