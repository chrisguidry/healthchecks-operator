package main

// The harness that the reconcile tests share: a fake Kubernetes API
// server, a fake Healthchecks project behind a proxy that counts the
// management API calls, one ClusterProject with its Secret, and a
// controller with a clock the test moves.

import (
	"bytes"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chrisguidry/healthchecks-operator/healthchecks/healthcheckstest"
)

const (
	operatorNamespace = "healthchecks-operator"
	projectKey        = "project-key"
)

// testClock is a clock that moves only when a test moves it.
type testClock struct {
	mutex sync.Mutex
	at    time.Time
}

func (c *testClock) now() time.Time {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.at
}

func (c *testClock) advance(by time.Duration) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.at = c.at.Add(by)
}

// syncBuffer is a log that the controller's goroutines write and the
// test reads.
type syncBuffer struct {
	mutex  sync.Mutex
	buffer bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.buffer.Write(p)
}

// lines returns every line written so far.
func (b *syncBuffer) lines() []string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return strings.Split(strings.TrimSpace(b.buffer.String()), "\n")
}

type harness struct {
	t     *testing.T
	api   *fakeKube
	hc    *healthcheckstest.Server
	clock *testClock
	log   *syncBuffer
	c     *controller
	// watching is the controller whose watches the harness runs.
	watching *controller
	// calls counts the requests that reached the management API.
	calls *atomic.Int32
	// pushover is the ID of the channel the project names.
	pushover string
	// refusal, when set, is the method and the body of a 400 that the
	// proxy answers every request of that method with.
	refusal atomic.Pointer[[2]string]
}

// startHarness seeds the project "internal" with its API key and a
// Pushover channel, and builds a controller that does not run until a
// test calls pass.
func startHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:     t,
		api:   startFakeKube(t),
		hc:    healthcheckstest.NewServer(projectKey),
		clock: &testClock{at: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)},
		calls: &atomic.Int32{},
	}
	t.Cleanup(h.hc.Close)
	h.pushover = h.hc.SeedChannel("Pushover", "po")
	h.hc.SeedChannel("Email", "email")

	target, err := url.Parse(h.hc.URL())
	mustSucceed(t, err)
	forward := httputil.NewSingleHostReverseProxy(target)
	// A request that a test's end cancels is not a failure to report.
	forward.ErrorLog = log.New(io.Discard, "", 0)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.calls.Add(1)
		if refusal := h.refusal.Load(); refusal != nil && r.Method == refusal[0] {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(refusal[1]))
			return
		}
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)

	h.api.create(secretsResource, secretWith(operatorNamespace, "internal-healthchecks", map[string][]byte{"api-key": []byte(projectKey)}))
	h.api.create(clusterProjectsResource, ClusterProject{
		Metadata: ObjectMeta{Name: "internal"},
		Spec: ClusterProjectSpec{
			URL:          proxy.URL,
			APIKeySecret: SecretKeyRef{Name: "internal-healthchecks", Key: "api-key"},
			Channels:     []string{"Pushover"},
		},
	})
	h.c = h.restart(settings{namespace: operatorNamespace})
	return h
}

// restart builds a new controller on the same cluster and the same
// Healthchecks, as a restarted operator would start, with an empty
// memory.
func (h *harness) restart(config settings) *controller {
	h.log = &syncBuffer{}
	return newController(config, h.api.client, newMetrics("dev"), h.clock.now, h.log)
}

// refuse answers every management API request of method from now on
// with a 400 and body, as Healthchecks answers a request it cannot
// accept. An upsert is a POST, and a delete is a DELETE.
func (h *harness) refuse(method, body string) {
	h.refusal.Store(&[2]string{method, body})
}

// pass runs one pass once the controller's stores hold what the fake
// API server holds, so a test's change reaches the pass the way a watch
// event would. The first pass lists and starts the watches.
func (h *harness) pass() {
	h.t.Helper()
	h.watch()
	eventually(h.t, "the stores to hold the cluster", h.synced)
	h.c.pass(h.t.Context())
}

// watch lists and starts the watches of the current controller, until
// the test ends, once for each controller.
func (h *harness) watch() {
	h.t.Helper()
	if h.watching == h.c {
		return
	}
	h.watching = h.c
	versions, err := h.c.list(h.t.Context())
	mustSucceed(h.t, err)
	var running sync.WaitGroup
	h.c.watch(h.t.Context(), &running, versions)
	h.t.Cleanup(running.Wait)
}

// synced reports whether every store holds each object at the version
// the fake API server holds.
func (h *harness) synced() bool {
	for resource, watch := range h.c.watches {
		if !maps.Equal(storedVersions(h.t, watch.store), h.api.versions(resource)) {
			return false
		}
	}
	return true
}

// runProbes runs the controller's probe schedule until the test ends.
func (h *harness) runProbes() {
	go h.c.schedule.run(h.t.Context())
}

func (h *harness) check(namespace, name string) Check {
	h.t.Helper()
	var check Check
	if !h.api.read(checksResource, namespace, name, &check) {
		h.t.Fatalf("Check %s/%s does not exist", namespace, name)
	}
	return check
}

func (h *harness) project(name string) ClusterProject {
	h.t.Helper()
	var project ClusterProject
	h.api.read(clusterProjectsResource, "", name, &project)
	return project
}

// editCheck changes a Check's spec, as kubectl edit would.
func (h *harness) editCheck(namespace, name string, change func(spec map[string]any)) {
	h.api.update(checksResource, namespace, name, func(object map[string]any) {
		change(object["spec"].(map[string]any))
	})
}

// httpCheck is an http Check against url, in the project "internal".
func httpCheck(namespace, name, url string) Check {
	return Check{
		Metadata: ObjectMeta{Namespace: namespace, Name: name},
		Spec: CheckSpec{
			ProjectRef:  ProjectRef{Kind: "ClusterProject", Name: "internal"},
			Description: "Public website",
			Tags:        []string{"internet", "website"},
			Grace:       "15m",
			HTTP: &HTTPProbe{
				Interval: "5m",
				Requests: []HTTPRequest{{URL: url, Expect: ResponseExpectation{Status: []int32{200}}}},
			},
		},
	}
}

// conditionOf returns a Check's condition of one type, or a condition
// with an empty status when there is none.
func conditionOf(check Check, kind string) Condition {
	found, _ := findCondition(check.Status.Conditions, kind)
	return found
}

// eventually waits until done reports true, and fails the test if it
// does not within testTimeout.
func eventually(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %s", what, testTimeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// pingKinds lists the kind and body of each ping a check received, as
// "kind" or "kind: body".
func (h *harness) pingKinds(uuid string) string {
	var kinds []string
	for _, ping := range h.hc.Pings(uuid) {
		kind := ping.Kind
		if ping.Body != "" {
			kind += ": " + ping.Body
		}
		kinds = append(kinds, kind)
	}
	return strings.Join(kinds, ", ")
}
