package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// startSite serves one page that answers with status.
func startSite(t *testing.T, status int) string {
	t.Helper()
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(site.Close)
	return site.URL + "/"
}

// probedCheck waits until the Check's Passing condition has a status,
// and returns the Check.
func (h *harness) probedCheck(namespace, name string) Check {
	h.t.Helper()
	eventually(h.t, "a probe result in status", func() bool {
		return conditionOf(h.check(namespace, name), "Passing").Status != ""
	})
	return h.check(namespace, name)
}

func TestAPassingProbePingsSuccess(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, httpCheck("example", "website", startSite(t, http.StatusOK)))
	h.runProbes()

	h.pass()

	check := h.probedCheck("example", "website")
	mustMatch(t, conditionOf(check, "Passing"), Condition{
		Type:               "Passing",
		Status:             ConditionTrue,
		ObservedGeneration: 1,
		Reason:             "ProbePassed",
		Message:            "the last probe passed",
		LastTransitionTime: "2026-09-27T12:00:00Z",
	})
	mustMatch(t, h.pingKinds(check.Status.UUID), "success")
}

// A Check's first result fails it at once, because Healthchecks does
// not time a check that has never had a success.
func TestAFirstProbeThatFailsPingsFailWithTheReason(t *testing.T) {
	h := startHarness(t)
	url := startSite(t, http.StatusBadGateway)
	h.api.create(checksResource, httpCheck("example", "website", url))
	h.runProbes()

	h.pass()

	check := h.probedCheck("example", "website")
	passing := conditionOf(check, "Passing")
	mustMatch(t, passing.Status, ConditionFalse)
	mustMatch(t, passing.Reason, "ProbeFailed")
	mustMatch(t, strings.HasPrefix(passing.Message, url), true)
	mustMatch(t, h.pingKinds(check.Status.UUID), "fail: "+passing.Message)
}

// A failure after a pass logs its reason and leaves the check up. The
// check goes down only when no success arrives within its timeout and
// grace, so a failure that the next probe recovers from never notifies.
func TestAFailureAfterAPassLogsTheReasonAndLeavesTheCheckUp(t *testing.T) {
	h := startHarness(t)
	url, site := switchSite(t, http.StatusOK)
	h.api.create(checksResource, httpCheck("example", "website", url))
	h.runProbes()
	h.pass()
	check := h.probedCheck("example", "website")

	site.Store(http.StatusBadGateway)
	h.c.schedule.expedite("example/website")
	eventually(t, "the failure in status", func() bool {
		return conditionOf(h.check("example", "website"), "Passing").Status == ConditionFalse
	})

	reason := conditionOf(h.check("example", "website"), "Passing").Message
	stored, _ := h.hc.Check("example-website")
	mustMatch(t, h.pingKinds(check.Status.UUID), "success, log: "+reason)
	mustMatch(t, stored.Status, "up")
	mustMatch(t, len(h.hc.Notifications(stored.UUID)), 0)
}

// The probe schedule's report writes status beside the pass, and each
// write states the whole status, so the pass's own fields stay.
func TestAProbeResultKeepsTheRestOfStatus(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, httpCheck("example", "website", startSite(t, http.StatusOK)))
	h.runProbes()
	h.pass()

	check := h.probedCheck("example", "website")
	h.pass()

	mustMatch(t, check.Status.Slug, "example-website")
	mustMatch(t, conditionOf(check, "Ready").Status, ConditionTrue)
	mustMatch(t, len(h.check("example", "website").Status.Conditions), 2)
}

// A tls probe of a server whose certificate no public root signed
// fails, and the reason names the verification error.
func TestATLSCheckPingsFailForACertificateThatDoesNotVerify(t *testing.T) {
	h := startHarness(t)
	server := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	check := httpCheck("example", "website-cert", "")
	check.Spec.HTTP = nil
	check.Spec.TLS = &TLSProbe{Interval: "24h", Host: strings.TrimPrefix(server.URL, "https://"), MinRemaining: "336h"}
	h.api.create(checksResource, check)
	h.runProbes()

	h.pass()

	got := h.probedCheck("example", "website-cert")
	stored, _ := h.hc.Check("example-website-cert")
	mustMatch(t, stored.Timeout.String(), "24h0m0s")
	mustMatch(t, conditionOf(got, "Passing").Status, ConditionFalse)
	mustMatch(t, h.pingKinds(got.Status.UUID), "fail: "+conditionOf(got, "Passing").Message)
}

// A tcp probe of a port that refuses fails, and the reason names the
// port and the refusal.
func TestATCPCheckPingsFailForAPortThatRefuses(t *testing.T) {
	h := startHarness(t)
	address := closedAddress(t)
	check := httpCheck("example", "ssh", "")
	check.Spec.HTTP = nil
	check.Spec.TCP = &TCPProbe{Interval: "5m", Connections: []TCPConnection{{Address: address}}}
	h.api.create(checksResource, check)
	h.runProbes()

	h.pass()

	got := h.probedCheck("example", "ssh")
	stored, _ := h.hc.Check("example-ssh")
	mustMatch(t, stored.Timeout.String(), "5m0s")
	mustMatch(t, got.Status.Probe, "tcp")
	mustMatch(t, conditionOf(got, "Passing").Message, address+": connect: connection refused")
	mustMatch(t, h.pingKinds(got.Status.UUID), "fail: "+address+": connect: connection refused")
}

// A dns probe of a name that does not exist fails, and the reason names
// the query and the response code.
func TestADNSCheckPingsFailForANameThatDoesNotExist(t *testing.T) {
	h := startHarness(t)
	server := serveDNS(t)
	check := httpCheck("example", "names", "")
	check.Spec.HTTP = nil
	check.Spec.DNS = &DNSProbe{Interval: "5m", Queries: []DNSQuery{{Server: server, Lookup: "missing.example.com"}}}
	h.api.create(checksResource, check)
	h.runProbes()

	h.pass()

	got := h.probedCheck("example", "names")
	stored, _ := h.hc.Check("example-names")
	reason := "missing.example.com A @" + server + ": NXDOMAIN"
	mustMatch(t, stored.Timeout.String(), "5m0s")
	mustMatch(t, got.Status.Probe, "dns")
	mustMatch(t, conditionOf(got, "Passing").Message, reason)
	mustMatch(t, h.pingKinds(got.Status.UUID), "fail: "+reason)
}

// A Check that becomes a cronJob check stops its probe, and loses the
// Passing condition that only a probe sets.
func TestACheckThatStopsProbingLosesPassing(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, httpCheck("example", "website", startSite(t, http.StatusOK)))
	h.runProbes()
	h.pass()
	h.probedCheck("example", "website")

	h.editCheck("example", "website", func(spec map[string]any) {
		delete(spec, "http")
		spec["cronJob"] = map[string]any{"name": "database-backup"}
	})
	h.pass()

	check := h.check("example", "website")
	mustMatch(t, conditionOf(check, "Passing").Status, "")
	mustMatch(t, conditionOf(check, "Ready").Reason, "CronJobNotFound")
}

// A probe check whose ping URL changes, because it took over another
// check by its slug, probes at once. The check it took over may have
// gone longer without a ping than the probe's interval allows, and a
// probe that waited out its interval would leave it down.
func TestACheckThatAdoptsAnotherCheckProbesAtOnce(t *testing.T) {
	h := startHarness(t)
	check := httpCheck("example", "website", startSite(t, http.StatusOK))
	check.Spec.HTTP.Interval = "24h"
	h.api.create(checksResource, check)
	h.runProbes()
	h.pass()
	h.probedCheck("example", "website")

	h.editCheck("example", "website", func(spec map[string]any) {
		spec["slug"] = "example-com"
	})
	h.pass()

	adopted, _ := h.hc.Check("example-com")
	eventually(t, "a ping on the adopted check", func() bool {
		return h.pingKinds(adopted.UUID) == "success"
	})
}
