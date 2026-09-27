package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadWatchEventsMovesTheResumeVersion(t *testing.T) {
	const (
		added    = `{"type":"ADDED","object":{"metadata":{"name":"gear","resourceVersion":"10"}}}`
		bookmark = `{"type":"BOOKMARK","object":{"metadata":{"resourceVersion":"11"}}}`
		gone     = `{"type":"ERROR","object":{"kind":"Status","code":410}}`
		later    = `{"type":"MODIFIED","object":{"metadata":{"name":"gear","resourceVersion":"99"}}}`
	)
	cases := []struct {
		name    string
		events  []string
		version string
		wakes   int
	}{
		{"a bookmark wakes nothing", []string{bookmark}, "11", 0},
		{"a change wakes the loop", []string{added}, "10", 1},
		{"an error ends the stream", []string{added, gone, later}, "10", 1},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			wake := make(chan struct{}, 1)
			decoder := json.NewDecoder(strings.NewReader(strings.Join(one.events, "\n")))

			mustMatch(t, readWatchEvents(decoder, "1", wake), one.version)
			mustMatch(t, len(wake), one.wakes)
		})
	}
}

// startWidgetWatch runs a watch on every namespace's widgets until the
// test ends, and returns the channel it wakes and the count of its
// restarts.
func startWidgetWatch(t *testing.T, client *kubeClient, resourceVersion string) (chan struct{}, *atomic.Int32) {
	t.Helper()
	wake := make(chan struct{}, 1)
	restarts := &atomic.Int32{}
	watch := newCollectionWatch(client, widgets, "", wake, func() { restarts.Add(1) })
	watch.pause = time.Millisecond
	watch.backoff = 4 * time.Millisecond
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		watch.run(t.Context(), resourceVersion)
	}()
	t.Cleanup(func() {
		select {
		case <-stopped:
		case <-time.After(testTimeout):
			t.Error("the watch did not stop")
		}
	})
	return wake, restarts
}

func waitForWake(t *testing.T, wake <-chan struct{}) {
	t.Helper()
	select {
	case <-wake:
	case <-time.After(testTimeout):
		t.Fatal("the watch did not wake the loop")
	}
}

// waitForWatchFrom waits until the fake API server has a watch request
// from resourceVersion.
func waitForWatchFrom(t *testing.T, api *fakeKube, resourceVersion string) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		for _, request := range api.requests() {
			if request.Query.Get("watch") == "true" && request.Query.Get("resourceVersion") == resourceVersion {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no watch from resourceVersion %s in %v", resourceVersion, api.requests())
}

func TestAChangeWakesTheLoop(t *testing.T) {
	api := startFakeKube(t)
	wake, _ := startWidgetWatch(t, api.client, "0")
	waitForWatchFrom(t, api, "0")

	api.create(widgets, newWidget("shop", "gear"))

	waitForWake(t, wake)
}

func TestTheWatchAsksForBookmarks(t *testing.T) {
	api := startFakeKube(t)
	startWidgetWatch(t, api.client, "0")
	waitForWatchFrom(t, api, "0")

	mustMatch(t, api.requests()[0].Query.Get("allowWatchBookmarks"), "true")
}

// A dropped stream lists the collection, wakes the loop, and watches
// again from the list's resourceVersion.
func TestADroppedWatchRelistsAndResumes(t *testing.T) {
	api := startFakeKube(t)
	wake, restarts := startWidgetWatch(t, api.client, "0")
	waitForWatchFrom(t, api, "0")
	api.create(widgets, newWidget("shop", "gear"))
	api.create(widgets, newWidget("shop", "bolt"))
	waitForWake(t, wake)

	api.dropWatches()

	waitForWake(t, wake)
	waitForWatchFrom(t, api, "2")
	mustMatch(t, requestCount(api, "/apis/example.com/v1/widgets", "false"), 1)
	mustMatch(t, requestCount(api, "/apis/example.com/v1/widgets", "true"), 2)
	mustMatch(t, restarts.Load(), 1)
}

// requestCount counts the lists, or the watches, of one collection.
func requestCount(api *fakeKube, path, watch string) int {
	count := 0
	for _, request := range api.requests() {
		if request.Path == path && (request.Query.Get("watch") == "true") == (watch == "true") {
			count++
		}
	}
	return count
}

// A 410 Gone means the API server no longer has the watch's version.
// The watch lists again for a current one.
func TestAGoneWatchRelistsAndResumes(t *testing.T) {
	api := startFakeKube(t)
	api.create(widgets, newWidget("shop", "gear"))
	api.create(widgets, newWidget("shop", "bolt"))
	api.compact()

	startWidgetWatch(t, api.client, "1")

	waitForWatchFrom(t, api, "2")
}

// A refused watch or a failed list is tried again, with a wait between
// tries.
func TestARefusedWatchIsTriedAgain(t *testing.T) {
	var watches atomic.Int32
	client := testKubeClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("watch") == "true" {
			watches.Add(1)
		}
		writeKubeStatus(w, http.StatusForbidden, "widgets is forbidden")
	})

	startWidgetWatch(t, client, "5")

	deadline := time.Now().Add(testTimeout)
	for watches.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	mustMatch(t, watches.Load() >= 3, true)
}
