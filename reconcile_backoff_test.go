package main

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAFailedUpsertBacksOffTheCheck(t *testing.T) {
	h := startHarness(t)
	h.refuse(http.MethodPost, `{"error": "unavailable"}`)
	h.api.create(checksResource, httpCheck("example", "website", "https://example.com/"))
	h.pass()
	calls, version := h.calls.Load(), h.check("example", "website").Metadata.ResourceVersion

	h.pass()
	h.pass()
	during := h.calls.Load() - calls
	h.clock.advance(5 * time.Second)
	h.pass()
	first := h.calls.Load() - calls
	h.clock.advance(5 * time.Second)
	h.pass()
	doubled := h.calls.Load() - calls
	h.clock.advance(5 * time.Second)
	h.pass()

	mustMatch(t, during, 0)
	mustMatch(t, first, 1)
	mustMatch(t, doubled, 1)
	mustMatch(t, h.calls.Load()-calls, 2)
	mustMatch(t, h.check("example", "website").Metadata.ResourceVersion, version)
}

func TestAFailedChannelListBacksOffTheProject(t *testing.T) {
	h := startHarness(t)
	h.api.update(secretsResource, operatorNamespace, "internal-healthchecks", func(object map[string]any) {
		object["data"] = map[string]any{"api-key": "d3Jvbmc="}
	})
	h.pass()
	calls := h.calls.Load()

	h.pass()
	during := h.calls.Load() - calls
	h.clock.advance(5 * time.Second)
	h.pass()

	mustMatch(t, during, 0)
	mustMatch(t, h.calls.Load()-calls, 1)
}

// A fault writes one line when it starts and one when it ends, however
// the text of the errors between changes.
func TestAFaultIsLoggedWhenItStartsAndWhenItEnds(t *testing.T) {
	h := startHarness(t)
	h.refuse(http.MethodPost, `{"error": "one"}`)
	h.api.create(checksResource, httpCheck("example", "website", "https://example.com/"))
	h.pass()
	h.refuse(http.MethodPost, `{"error": "two"}`)
	h.clock.advance(5 * time.Second)
	h.pass()
	h.refusal.Store(nil)
	h.clock.advance(10 * time.Second)

	h.pass()

	mustMatch(t, strings.Join(h.log.lines(), "\n"), strings.Join([]string{
		"check example/website: added the finalizer healthchecks.guid.foo/check",
		`check example/website: upserting check example-website: healthchecks: Bad Request: {"error": "one"}`,
		"check example/website: created check example-website in Healthchecks",
		"check example/website: recovered",
	}, "\n"))
}

// refusePings is a transport that fails every ping while down is set,
// and sends every other request on.
type refusePings struct {
	down *atomic.Bool
}

func (r refusePings) RoundTrip(request *http.Request) (*http.Response, error) {
	if r.down.Load() && strings.HasPrefix(request.URL.Path, "/ping/") {
		return nil, errors.New("connection refused")
	}
	return http.DefaultTransport.RoundTrip(request)
}

// The pings for one Job go out in order after the backoff, from the
// first one that failed.
func TestFailedCronJobPingsWaitForTheBackoff(t *testing.T) {
	h := startHarness(t)
	down := &atomic.Bool{}
	h.c.http = &http.Client{Transport: refusePings{down}}
	h.api.create(cronJobsResource, backupCronJob())
	h.api.create(checksResource, backupCheck())
	h.pass()
	uuid := h.check("example", "database-backup").Status.UUID
	h.startRun("backup-1")
	h.finishRun("backup-1", "Complete")
	down.Store(true)
	h.pass()
	down.Store(false)

	h.pass()
	during := h.pingKinds(uuid)
	h.clock.advance(5 * time.Second)
	h.pass()

	mustMatch(t, during, "")
	mustMatch(t, h.pingKinds(uuid), "success")
	mustMatch(t, h.check("example", "database-backup").Status.LastReportedJob, "backup-1")
	mustMatch(t, h.log.lines()[2], `check example/database-backup: pinging for Job backup-1: Post "`+h.hc.URL()+"/ping/"+uuid+`": connection refused`)
	mustMatch(t, h.log.lines()[3], "check example/database-backup: ping recovered")
}

// refusePingsAfter lets the first allowed pings through and refuses
// every ping after them.
type refusePingsAfter struct {
	allowed *atomic.Int32
}

func (r refusePingsAfter) RoundTrip(request *http.Request) (*http.Response, error) {
	if strings.HasPrefix(request.URL.Path, "/ping/") && r.allowed.Add(-1) < 0 {
		return nil, errors.New("connection refused")
	}
	return http.DefaultTransport.RoundTrip(request)
}

// A ping that fails partway through a pass keeps the place of the last
// run reported before it, so the pass after the backoff sends no run's
// finish ping a second time.
func TestAFailedPingKeepsThePlaceOfTheRunsReportedBeforeIt(t *testing.T) {
	h := startHarness(t)
	h.api.now = h.clock.now
	allowed := &atomic.Int32{}
	allowed.Store(1)
	h.c.http = &http.Client{Transport: refusePingsAfter{allowed}}
	h.api.create(cronJobsResource, backupCronJob())
	h.api.create(checksResource, backupCheck())
	h.startRun("backup-1")
	h.finishRun("backup-1", "Complete")
	h.pass()
	uuid := h.check("example", "database-backup").Status.UUID
	h.clock.advance(time.Minute)
	h.startRun("backup-2")
	h.finishRun("backup-2", "Complete")
	h.clock.advance(time.Minute)
	h.startRun("backup-3")
	allowed.Store(2)
	h.pass()

	allowed.Store(100)
	h.clock.advance(5 * time.Second)
	h.pass()

	mustMatch(t, h.pingKinds(uuid), "success, start, success, start")
}

// The local port changes on every connection, so the text leaves the
// addresses out, and a failure that repeats writes the same message.
func TestANetworkErrorLeavesOutTheAddresses(t *testing.T) {
	err := &url.Error{Op: "Post", URL: "http://healthchecks.example.com/api/v3/checks/", Err: &net.OpError{
		Op:     "read",
		Net:    "tcp",
		Source: &net.TCPAddr{IP: net.IPv4(10, 42, 0, 5), Port: 54321},
		Addr:   &net.TCPAddr{IP: net.IPv4(10, 43, 0, 10), Port: 8000},
		Err:    errors.New("read: connection reset by peer"),
	}}

	mustMatch(t, errorText(err), `Post "http://healthchecks.example.com/api/v3/checks/": read tcp: read: connection reset by peer`)
}
