package main

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAnHTTPCheckBecomesACheckInHealthchecks(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, httpCheck("example", "website", "https://example.com/"))

	h.pass()

	stored, found := h.hc.Check("example-website")
	mustMatch(t, found, true)
	mustMatch(t, stored.Name, "example/website")
	mustMatch(t, stored.Description, "Public website")
	mustMatch(t, strings.Join(stored.Tags, " "), "internet website")
	mustMatch(t, stored.Grace, 15*time.Minute)
	mustMatch(t, stored.Timeout, 5*time.Minute)
	mustMatch(t, strings.Join(stored.Channels, ","), h.pushover)
	check := h.check("example", "website")
	mustMatch(t, check.Status.Slug, "example-website")
	mustMatch(t, check.Status.UUID, stored.UUID)
	mustMatch(t, check.Status.PingURL, stored.PingURL)
	mustMatch(t, slices.Equal(check.Metadata.Finalizers, []string{checkFinalizer}), true)
	mustMatch(t, conditionOf(check, "Ready"), Condition{
		Type:               "Ready",
		Status:             ConditionTrue,
		ObservedGeneration: 1,
		Reason:             "Synced",
		Message:            "the check exists in Healthchecks and matches the spec",
		LastTransitionTime: "2026-09-27T12:00:00Z",
	})
	mustMatch(t, strings.Join(h.log.lines(), "\n"), strings.Join([]string{
		"check example/website: added the finalizer healthchecks.guid.foo/check",
		"check example/website: created check example-website in Healthchecks",
	}, "\n"))
}

func TestACheckWithItsOwnFieldsOverridesTheDefaults(t *testing.T) {
	h := startHarness(t)
	email := h.hc.SeedChannel("Pager", "email")
	check := httpCheck("example", "website", "https://example.com/")
	check.Spec.DisplayName = "example.com"
	check.Spec.Slug = "example-site"
	check.Spec.Grace = ""
	check.Spec.Channels = []string{"Pager"}
	h.api.create(checksResource, check)

	h.pass()

	stored, _ := h.hc.Check("example-site")
	mustMatch(t, stored.Name, "example.com")
	mustMatch(t, stored.Grace, time.Hour)
	mustMatch(t, strings.Join(stored.Channels, ","), email)
}

func TestASteadyPassCallsNothingAndWritesNothing(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, httpCheck("example", "website", "https://example.com/"))
	h.pass()
	calls, requests := h.calls.Load(), len(h.api.requests())

	h.pass()

	mustMatch(t, h.calls.Load(), calls)
	patches := 0
	for _, request := range h.api.requests()[requests:] {
		if request.Method == "PATCH" {
			patches++
		}
	}
	mustMatch(t, patches, 0)
}

func TestAChangedSpecUpdatesTheCheck(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, httpCheck("example", "website", "https://example.com/"))
	h.pass()

	h.editCheck("example", "website", func(spec map[string]any) { spec["description"] = "The public website" })
	h.pass()

	stored, _ := h.hc.Check("example-website")
	mustMatch(t, stored.Description, "The public website")
	mustMatch(t, conditionOf(h.check("example", "website"), "Ready").ObservedGeneration, 2)
	mustMatch(t, h.log.lines()[2], "check example/website: updated check example-website in Healthchecks")
}

// A restarted operator holds no memory of what it sent, so it sends
// one upsert for each Check. Healthchecks finds the check by its slug,
// and nothing a person caused happened, so nothing is logged.
func TestARestartUpsertsEachCheckOnceAndLogsNothing(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, httpCheck("example", "website", "https://example.com/"))
	h.pass()
	uuid := h.check("example", "website").Status.UUID
	h.c = h.restart(settings{namespace: operatorNamespace})
	calls := h.calls.Load()

	h.pass()
	h.pass()

	// One call lists the project's channels, and one upserts the check.
	mustMatch(t, h.calls.Load()-calls, 2)
	mustMatch(t, h.check("example", "website").Status.UUID, uuid)
	mustMatch(t, strings.Join(h.log.lines(), "\n"), "")
}

func TestAChangedSlugDeletesTheCheckAtTheOldSlug(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, httpCheck("example", "website", "https://example.com/"))
	h.pass()

	h.editCheck("example", "website", func(spec map[string]any) { spec["slug"] = "example-site" })
	h.pass()

	_, old := h.hc.Check("example-website")
	mustMatch(t, old, false)
	stored, found := h.hc.Check("example-site")
	mustMatch(t, found, true)
	check := h.check("example", "website")
	mustMatch(t, check.Status.Slug, "example-site")
	mustMatch(t, check.Status.UUID, stored.UUID)
	mustMatch(t, h.log.lines()[2], "check example/website: deleted check example-website from Healthchecks, because the slug changed to example-site")
	mustMatch(t, h.log.lines()[3], "check example/website: created check example-site in Healthchecks")
}

// Healthchecks' own answer is the message, word for word, the way it
// explains a schedule that it cannot parse.
func TestACheckThatHealthchecksRefusesIsNotReady(t *testing.T) {
	h := startHarness(t)
	h.refuse(http.MethodPost, `{"error": "json validation error: schedule is not a valid cron expression"}`)
	h.api.create(checksResource, httpCheck("example", "website", "https://example.com/"))

	h.pass()

	ready := conditionOf(h.check("example", "website"), "Ready")
	mustMatch(t, ready.Reason, "UpsertFailed")
	mustMatch(t, ready.Message, `upserting check example-website: healthchecks: Bad Request: {"error": "json validation error: schedule is not a valid cron expression"}`)
}

func TestACheckIsNotReadyWhileItsProjectIsNot(t *testing.T) {
	cases := []struct {
		name    string
		project string
		reason  string
		message string
	}{
		{"missing", "outside", "ProjectNotFound", "ClusterProject outside does not exist"},
		{"failing", "broken", "ProjectNotReady", `ClusterProject broken is not ready: reading key "api-key" of Secret healthchecks-operator/missing: not found`},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			h := startHarness(t)
			h.api.create(clusterProjectsResource, ClusterProject{
				Metadata: ObjectMeta{Name: "broken"},
				Spec:     ClusterProjectSpec{URL: h.hc.URL(), APIKeySecret: SecretKeyRef{Name: "missing", Key: "api-key"}},
			})
			check := httpCheck("example", "website", "https://example.com/")
			check.Spec.ProjectRef.Name = one.project
			h.api.create(checksResource, check)

			h.pass()

			got := h.check("example", "website")
			mustMatch(t, conditionOf(got, "Ready").Reason, one.reason)
			mustMatch(t, conditionOf(got, "Ready").Message, one.message)
			mustMatch(t, len(got.Metadata.Finalizers), 0)
			mustMatch(t, h.log.lines()[len(h.log.lines())-1], "check example/website: "+one.message)
		})
	}
}

func TestACheckNamingAChannelThatDoesNotExistIsNotReady(t *testing.T) {
	h := startHarness(t)
	check := httpCheck("example", "website", "https://example.com/")
	check.Spec.Channels = []string{"Slack"}
	h.api.create(checksResource, check)
	h.pass()
	h.hc.SeedChannel("Slack", "slack")

	before := conditionOf(h.check("example", "website"), "Ready")
	h.pass()

	mustMatch(t, before.Reason, "ChannelNotFound")
	mustMatch(t, before.Message, `no channel in the project is named "Slack"`)
	mustMatch(t, conditionOf(h.check("example", "website"), "Ready").Reason, "Synced")
}

// A failure that lasts writes one line, however many passes see it.
func TestAFailureThatLastsIsLoggedOnce(t *testing.T) {
	h := startHarness(t)
	check := httpCheck("example", "website", "https://example.com/")
	check.Spec.ProjectRef.Name = "outside"
	h.api.create(checksResource, check)

	h.pass()
	h.pass()
	h.pass()

	mustMatch(t, strings.Join(h.log.lines(), "\n"), "check example/website: ClusterProject outside does not exist")
}

// The check at the old slug goes before the check at the new one is
// made. A delete that fails leaves status at the old slug, so the next
// pass sends the delete again, and there is never a second check for
// one Check.
func TestAChangedSlugWaitsForTheOldCheckToGo(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, httpCheck("example", "website", "https://example.com/"))
	h.pass()
	h.refuse(http.MethodDelete, `{"error": "not now"}`)

	h.editCheck("example", "website", func(spec map[string]any) { spec["slug"] = "example-site" })
	h.pass()

	_, created := h.hc.Check("example-site")
	mustMatch(t, created, false)
	check := h.check("example", "website")
	mustMatch(t, check.Status.Slug, "example-website")
	mustMatch(t, conditionOf(check, "Ready").Message, `deleting the check at the old slug example-website: healthchecks: Bad Request: {"error": "not now"}`)
}

// A Check deleted and made again under the same name is a new object,
// and the operator holds nothing over from the old one.
func TestACheckMadeAgainUnderItsNameStartsOver(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, httpCheck("example", "website", "https://example.com/"))
	h.pass()
	h.api.update(checksResource, "example", "website", func(object map[string]any) {
		delete(metadataOf(object), "finalizers")
	})
	h.api.delete(checksResource, "example", "website")
	h.api.create(checksResource, httpCheck("example", "website", "https://example.com/"))

	h.pass()

	stored, _ := h.hc.Check("example-website")
	mustMatch(t, h.check("example", "website").Status.UUID, stored.UUID)
}

// A pass that holds an old version of a Check gets a 409 on the
// finalizer patch, and leaves the Check alone without a retry. The
// newer version's event reaches the store, and the pass after it adds
// the finalizer.
func TestAConflictWaitsForTheNewerCheck(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, httpCheck("example", "website", "https://example.com/"))
	h.watch()
	eventually(t, "the store to hold the Check", h.synced)
	store := h.c.watches[checksResource].store
	stale := store.snapshot()[0]
	h.editCheck("example", "website", func(spec map[string]any) { spec["description"] = "The website" })
	eventually(t, "the store to hold the edit", h.synced)
	store.put("example/website", stale)
	path := checksResource.path("example", "website")

	h.c.pass(t.Context())
	refused := h.api.requestCount("PATCH", path, false)
	unchanged := h.check("example", "website").Metadata.holds(checkFinalizer)
	h.editCheck("example", "website", func(spec map[string]any) { spec["description"] = "Our website" })
	h.pass()

	mustMatch(t, refused, 1)
	mustMatch(t, unchanged, false)
	mustMatch(t, h.api.requestCount("PATCH", path, false), 2)
	mustMatch(t, h.check("example", "website").Metadata.holds(checkFinalizer), true)
}

// status.probe names the probe kind, because a printer column is a
// JSONPath and cannot tell which of http, tls, cronJob, and ping is
// set.
func TestAPassRecordsWhichProbeACheckRuns(t *testing.T) {
	tlsCheck := httpCheck("example", "website", "")
	tlsCheck.Spec.HTTP = nil
	tlsCheck.Spec.TLS = &TLSProbe{Interval: "24h", Host: "example.com", MinRemaining: "336h"}
	cases := []struct {
		check Check
		want  string
	}{
		{httpCheck("example", "website", "https://example.com/"), "http"},
		{tlsCheck, "tls"},
		{backupCheck(), "cronJob"},
		{pingCheck(PingProbe{Timeout: "24h"}), "ping"},
	}
	for _, one := range cases {
		t.Run(one.want, func(t *testing.T) {
			h := startHarness(t)
			h.api.create(cronJobsResource, backupCronJob())
			h.api.create(checksResource, one.check)

			h.pass()

			mustMatch(t, h.check(one.check.Metadata.Namespace, one.check.Metadata.Name).Status.Probe, one.want)
		})
	}
}
