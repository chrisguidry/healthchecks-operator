package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
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

// passCount reads how many passes the controller finished.
func passCount(t *testing.T, c *controller) int {
	t.Helper()
	for _, sample := range scrape(t, c.readings) {
		if count, found := strings.CutPrefix(sample, "healthchecks_reconcile_duration_seconds_count "); found {
			passes, err := strconv.Atoi(count)
			mustSucceed(t, err)
			return passes
		}
	}
	t.Fatal("no healthchecks_reconcile_duration_seconds_count sample")
	return 0
}

// The backstop tick runs a pass when nothing changes, which is what
// keeps the heartbeat going. The passes read the stores, so the lists
// that fill them before the first pass are the only lists.
func TestTheBackstopTickRunsAPassWithNoList(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, httpCheck("example", "website", "https://example.com/"))
	h.c.backstop = time.Millisecond
	runController(t, h.c)

	eventually(t, "three passes", func() bool { return passCount(t, h.c) > 3 })
	for _, resource := range watchedCollections {
		mustMatch(t, h.api.requestCount("GET", resource.path("", ""), false), 1)
	}
}

// A Check made, changed, and deleted in the cluster reaches the next
// pass through the watch, with no list.
func TestAPassSeesEachChangeThroughTheWatch(t *testing.T) {
	h := startHarness(t)
	h.pass()
	h.api.create(checksResource, httpCheck("example", "website", "https://example.com/"))

	h.pass()
	created, _ := h.hc.Check("example-website")
	h.editCheck("example", "website", func(spec map[string]any) { spec["description"] = "The website" })
	h.pass()
	updated, _ := h.hc.Check("example-website")
	h.api.delete(checksResource, "example", "website")
	h.pass()
	h.pass()

	mustMatch(t, created.Description, "Public website")
	mustMatch(t, updated.Description, "The website")
	mustMatch(t, h.api.read(checksResource, "example", "website", &Check{}), false)
	_, found := h.hc.Check("example-website")
	mustMatch(t, found, false)
	mustMatch(t, h.api.requestCount("GET", checksResource.path("", ""), false), 1)
}

// While the first list is refused, the loop keeps trying and makes no
// pass, and it stops cleanly when its context ends, as on SIGTERM.
func TestTheLoopMakesNoPassWhileTheFirstListIsRefused(t *testing.T) {
	var requests, writes atomic.Int32
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(refusing.Close)
	client := newKubeClient(refusing.URL, refusing.Client(), "")
	c := newController(settings{namespace: operatorNamespace}, client, newMetrics("dev"), time.Now, &syncBuffer{})
	for _, watch := range c.watches {
		watch.pause, watch.backoff = time.Millisecond, time.Millisecond
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	err := c.run(ctx)

	mustSucceed(t, err)
	mustMatch(t, requests.Load() > 1, true)
	mustMatch(t, writes.Load(), int32(0))
}

// A pass that cannot decode a store writes one line, and reconciles
// nothing. A store that trims to the pass's type always decodes, so the
// test gives the Checks a store that keeps whole objects.
func TestAPassThatCannotReadTheClusterSaysSo(t *testing.T) {
	h := startHarness(t)
	h.c.watches[checksResource].store, _ = newTestStore(func(object json.RawMessage) (json.RawMessage, error) { return object, nil })
	h.api.create(checksResource, map[string]any{
		"metadata": map[string]any{"namespace": "example", "name": "website"},
		"spec":     "not an object",
	})

	h.pass()
	h.pass()

	mustMatch(t, strings.Join(h.log.lines(), "\n"), "reading the cluster: decoding checks.healthchecks.guid.foo: json: cannot unmarshal string into Go struct field Check.spec of type main.CheckSpec")
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

// A first list the API server refuses, as it refuses a collection whose
// RBAC rule has not arrived yet, is tried again until it works, instead
// of ending the operator.
func TestTheFirstListIsTriedAgainUntilItWorks(t *testing.T) {
	h := startHarness(t)
	for _, watch := range h.c.watches {
		watch.pause, watch.backoff = time.Millisecond, time.Millisecond
	}
	h.api.refuse(http.MethodGet, "/api/v1/configmaps", http.StatusForbidden)
	listed := make(chan error, 1)

	go func() {
		_, err := h.c.list(t.Context())
		listed <- err
	}()
	eventually(t, "a refused list of configmaps", func() bool {
		return h.api.requestCount(http.MethodGet, "/api/v1/configmaps", false) >= 2
	})
	h.api.allow(http.MethodGet, "/api/v1/configmaps")

	select {
	case err := <-listed:
		mustSucceed(t, err)
	case <-time.After(testTimeout):
		t.Fatal("the first list did not finish after the refusal ended")
	}
}

// A first list that cannot work however often it is tried, such as one
// of a resource the API server does not serve, ends the operator, so
// the failure shows in the pod's restarts.
func TestAFirstListOfAMissingResourceEndsTheOperator(t *testing.T) {
	missing := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(missing.Close)
	client := newKubeClient(missing.URL, missing.Client(), "")
	c := newController(settings{namespace: operatorNamespace}, client, newMetrics("dev"), time.Now, &syncBuffer{})

	err := c.run(t.Context())

	mustMatch(t, errors.Is(err, errNotFound), true)
}
