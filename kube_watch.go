package main

// A watch is a GET whose response does not end. The API server holds
// the connection open and writes one JSON event for each change. The
// reconcile loop lists everything on each pass, so an event only wakes
// the loop and carries nothing to it.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// collectionWatch wakes the reconcile loop on every change to one
// collection.
type collectionWatch struct {
	client    *kubeClient
	resource  kubeResource
	namespace string
	wake      chan<- struct{}
	// restarted runs each time the watch opens a stream after the first,
	// so a metric counts the restarts.
	restarted func()

	// pause is the wait after a stream ends, before the list. backoff is
	// the longest wait after failures in a row, which double the pause
	// each time. They are fields so a test runs in milliseconds.
	pause   time.Duration
	backoff time.Duration
}

func newCollectionWatch(client *kubeClient, resource kubeResource, namespace string, wake chan<- struct{}, restarted func()) *collectionWatch {
	return &collectionWatch{
		client:    client,
		resource:  resource,
		namespace: namespace,
		wake:      wake,
		restarted: restarted,
		pause:     2 * time.Second,
		backoff:   time.Minute,
	}
}

// run watches from resourceVersion until ctx ends. When a stream ends,
// for a dropped connection or a 410 Gone, the watch lists the
// collection, wakes the loop, and watches again from the list's
// resourceVersion. The pass that the wake starts reads every change
// that the gap held.
//
// A refused watch or a failed list writes one line when the reason
// changes, and one line when the watch works again. The wait doubles
// with each failure in a row, so a fault that lasts costs one request
// a minute.
func (w *collectionWatch) run(ctx context.Context, resourceVersion string) {
	wait := w.pause
	fault := ""
	failed := func(reason string) {
		if reason != fault {
			fmt.Fprintf(os.Stderr, "watching %s: %s\n", w.resource, reason)
		}
		fault = reason
		wait = min(wait*2, w.backoff)
	}
	for first := true; ctx.Err() == nil; first = false {
		if !first {
			w.restarted()
		}
		refused := w.stream(ctx, &resourceVersion)
		if !sleep(ctx, wait) {
			return
		}
		if refused != "" {
			failed(refused)
			continue
		}
		version, err := w.list(ctx)
		if err != nil {
			failed("listing to resume: " + err.Error())
			continue
		}
		if fault != "" {
			fmt.Fprintf(os.Stderr, "watching %s again\n", w.resource)
		}
		fault, wait = "", w.pause
		resourceVersion = version
		poke(w.wake)
	}
}

// stream opens one watch and reads it to its end, and moves
// resourceVersion to the last event. It returns the reason the API
// server refused the watch, or an empty string. A 410 Gone is not a
// refusal: the list that follows gives a current resourceVersion.
func (w *collectionWatch) stream(ctx context.Context, resourceVersion *string) string {
	resp, err := w.client.watch(ctx, w.resource, w.namespace, *resourceVersion)
	if err != nil {
		return err.Error()
	}
	// The body is closed, not drained, because a drain would wait on a
	// stream that the server keeps open.
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		*resourceVersion = readWatchEvents(json.NewDecoder(resp.Body), *resourceVersion, w.wake)
		return ""
	case http.StatusGone:
		return ""
	}
	return resp.Status + ": " + responseText(resp.Body)
}

func (w *collectionWatch) list(ctx context.Context) (string, error) {
	var list struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := w.client.list(ctx, w.resource, w.namespace, &list); err != nil {
		return "", err
	}
	return list.Metadata.ResourceVersion, nil
}

// readWatchEvents reads events until the stream ends and returns the
// resourceVersion of the last one. An ERROR event ends the stream: the
// API server sends a 410 Gone this way when it no longer has the
// version the watch started from. A BOOKMARK moves the version and
// wakes nothing.
func readWatchEvents(decoder *json.Decoder, resourceVersion string, wake chan<- struct{}) string {
	for {
		var event struct {
			Type   string `json:"type"`
			Object struct {
				Metadata struct {
					ResourceVersion string `json:"resourceVersion"`
				} `json:"metadata"`
			} `json:"object"`
		}
		if err := decoder.Decode(&event); err != nil || event.Type == "ERROR" {
			return resourceVersion
		}
		if version := event.Object.Metadata.ResourceVersion; version != "" {
			resourceVersion = version
		}
		if event.Type != "BOOKMARK" {
			poke(wake)
		}
	}
}

// poke wakes the loop without blocking. The channel has a buffer of
// one, so many changes before the next pass wake it once.
func poke(wake chan<- struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

// sleep waits and returns false if ctx ended first.
func sleep(ctx context.Context, wait time.Duration) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
