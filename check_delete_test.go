package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/chrisguidry/healthchecks-operator/healthchecks"
)

func TestADeletedCheckIsDeletedFromHealthchecks(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, httpCheck("example", "website", "https://example.com/"))
	h.pass()

	h.api.delete(checksResource, "example", "website")
	h.pass()

	_, found := h.hc.Check("example-website")
	mustMatch(t, found, false)
	mustMatch(t, h.api.read(checksResource, "example", "website", &Check{}), false)
	mustMatch(t, h.log.lines()[2], "check example/website: deleted check example-website from Healthchecks")
	mustMatch(t, h.log.lines()[3], "check example/website: removed the finalizer healthchecks.guid.foo/check")
}

// An operator that stopped between its upsert and its status write
// left a check that status does not name. The slug finds it.
func TestADeletedCheckWithNoUUIDIsFoundBySlug(t *testing.T) {
	h := startHarness(t)
	client := healthchecks.NewClient(h.hc.URL(), projectKey, nil)
	_, err := client.Upsert(t.Context(), healthchecks.UpsertRequest{Slug: "example-website", Period: healthchecks.FixedTimeout(time.Hour)})
	mustSucceed(t, err)
	check := httpCheck("example", "website", "https://example.com/")
	check.Metadata.Finalizers = []string{checkFinalizer}
	h.api.create(checksResource, check)
	h.api.delete(checksResource, "example", "website")

	h.pass()

	_, found := h.hc.Check("example-website")
	mustMatch(t, found, false)
	mustMatch(t, h.api.read(checksResource, "example", "website", &Check{}), false)
}

func TestADeletedCheckKeepsItsFinalizerWhileItsProjectCannotDeleteIt(t *testing.T) {
	cases := []struct {
		name    string
		fault   func(h *harness)
		reason  string
		message string
	}{
		{
			"missing",
			func(h *harness) { h.api.delete(clusterProjectsResource, "", "internal") },
			"ProjectNotFound",
			"ClusterProject internal does not exist, so the check stays in Healthchecks and the finalizer stays",
		},
		{
			"failing",
			func(h *harness) { h.api.delete(secretsResource, operatorNamespace, "internal-healthchecks") },
			"ProjectNotReady",
			`ClusterProject internal is not ready, so the check stays in Healthchecks and the finalizer stays: reading key "api-key" of Secret healthchecks-operator/internal-healthchecks: not found`,
		},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			h := startHarness(t)
			h.api.create(checksResource, httpCheck("example", "website", "https://example.com/"))
			h.pass()
			one.fault(h)

			h.api.delete(checksResource, "example", "website")
			h.pass()

			_, found := h.hc.Check("example-website")
			mustMatch(t, found, true)
			check := h.check("example", "website")
			mustMatch(t, check.Metadata.holds(checkFinalizer), true)
			mustMatch(t, conditionOf(check, "Ready").Reason, one.reason)
			mustMatch(t, conditionOf(check, "Ready").Message, one.message)
		})
	}
}

// Healthchecks' own answer is the message when the delete fails, and
// the finalizer stays until a pass deletes the check.
func TestADeletedCheckKeepsItsFinalizerWhenTheDeleteFails(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, httpCheck("example", "website", "https://example.com/"))
	h.pass()
	h.refuse(http.MethodDelete, `{"error": "not now"}`)

	h.api.delete(checksResource, "example", "website")
	h.pass()

	ready := conditionOf(h.check("example", "website"), "Ready")
	mustMatch(t, ready.Reason, "DeleteFailed")
	mustMatch(t, ready.Message, `deleting check example-website: healthchecks: Bad Request: {"error": "not now"}`)
}
