package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// pingCheck is a ping Check that the database backup pings on its own,
// with no ConfigMap.
func pingCheck(ping PingProbe) Check {
	return Check{
		Metadata: ObjectMeta{Namespace: "example", Name: "database-backup"},
		Spec: CheckSpec{
			ProjectRef:  ProjectRef{Kind: "ClusterProject", Name: "internal"},
			Description: "Nightly database backup",
			Ping:        &ping,
		},
	}
}

func TestAPingCheckSetsItsPeriod(t *testing.T) {
	cases := []struct {
		name     string
		ping     PingProbe
		schedule string
		timezone string
		timeout  time.Duration
	}{
		{"a schedule in a time zone", PingProbe{Schedule: "0 3 * * *", TimeZone: "America/New_York"}, "0 3 * * *", "America/New_York", 0},
		{"a macro schedule in UTC", PingProbe{Schedule: "@daily"}, "0 0 * * *", "UTC", 0},
		{"a timeout", PingProbe{Timeout: "24h"}, "", "", 24 * time.Hour},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			h := startHarness(t)
			h.api.create(checksResource, pingCheck(one.ping))

			h.pass()

			stored, _ := h.hc.Check("example-database-backup")
			mustMatch(t, stored.Schedule, one.schedule)
			mustMatch(t, stored.Timezone, one.timezone)
			mustMatch(t, stored.Timeout, one.timeout)
			check := h.check("example", "database-backup")
			mustMatch(t, conditionOf(check, "Ready").Status, ConditionTrue)
			mustMatch(t, conditionOf(check, "Passing").Status, "")
			mustMatch(t, check.Status.Probe, "ping")
		})
	}
}

// The CRD refuses a timeout that is not a duration. The operator does
// not depend on that rule, and says why in Ready.
func TestAPingCheckWithATimeoutThatIsNotADurationIsNotReady(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, pingCheck(PingProbe{Timeout: "daily"}))

	h.pass()

	ready := conditionOf(h.check("example", "database-backup"), "Ready")
	mustMatch(t, ready.Reason, "InvalidSpec")
	mustMatch(t, ready.Message, `ping.timeout: time: invalid duration "daily"`)
}

// A ping check sends no ping of its own, so the check in Healthchecks
// hears only from the workload.
func TestAPingCheckSendsNoPing(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, pingCheck(PingProbe{Timeout: "1m"}))
	h.runProbes()

	h.pass()
	h.pass()

	mustMatch(t, h.pingKinds(h.check("example", "database-backup").Status.UUID), "")
}

// backupConfigMap names the ConfigMap the backup reads its ping URL
// from.
func backupConfigMap(name, key string) PingProbe {
	return PingProbe{Schedule: "0 3 * * *", ConfigMap: &PingConfigMap{Name: name, Key: key}}
}

// heldConfigMap reads a ConfigMap from the fake API server, and fails
// the test when it does not exist.
func (h *harness) heldConfigMap(name string) configMap {
	h.t.Helper()
	var held configMap
	if !h.api.read(configMapsResource, "example", name, &held) {
		h.t.Fatalf("ConfigMap example/%s does not exist", name)
	}
	return held
}

// A ConfigMap that Terraform made has no label and no owner. The
// operator takes it over: it writes the ping URL, the label, and an
// owner reference to the Check.
func TestAPingCheckTakesOverAConfigMap(t *testing.T) {
	h := startHarness(t)
	h.api.create(configMapsResource, map[string]any{
		"metadata": map[string]any{"namespace": "example", "name": "database-backup-healthcheck"},
		"data":     map[string]any{"HEALTHCHECK_URL": "https://hc-ping.example.net/old"},
	})
	h.api.create(checksResource, pingCheck(backupConfigMap("database-backup-healthcheck", "")))

	h.pass()

	check := h.check("example", "database-backup")
	held := h.heldConfigMap("database-backup-healthcheck")
	mustMatchMap(t, held.Data, map[string]string{"HEALTHCHECK_URL": check.Status.PingURL})
	mustMatchMap(t, held.Metadata.Labels, map[string]string{"app.kubernetes.io/managed-by": "healthchecks-operator"})
	mustMatch(t, held.controlledBy(check.Metadata.UID), true)
	mustMatch(t, check.Status.ConfigMap, "database-backup-healthcheck")
	mustMatch(t, strings.HasPrefix(check.Status.PingURL, h.hc.URL()), true)
}

// The owner reference names the Check as the controller, and does not
// block the Check's deletion, so the garbage collector deletes the
// ConfigMap after the Check goes.
func TestAPingCheckOwnsItsConfigMap(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, pingCheck(backupConfigMap("database-backup-healthcheck", "")))

	h.pass()

	var held struct {
		Metadata struct {
			OwnerReferences []ownerReferenceApply `json:"ownerReferences"`
		} `json:"metadata"`
	}
	h.api.read(configMapsResource, "example", "database-backup-healthcheck", &held)
	check := h.check("example", "database-backup")
	mustMatch(t, len(held.Metadata.OwnerReferences), 1)
	mustMatch(t, held.Metadata.OwnerReferences[0], ownerReferenceApply{
		APIVersion:         "healthchecks.guid.foo/v1alpha1",
		Kind:               "Check",
		Name:               "database-backup",
		UID:                check.Metadata.UID,
		Controller:         true,
		BlockOwnerDeletion: false,
	})
}

func TestAPingCheckWritesTheKeyItNames(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, pingCheck(backupConfigMap("database-backup-healthcheck", "PING_URL")))
	h.pass()

	h.editCheck("example", "database-backup", func(spec map[string]any) {
		spec["ping"].(map[string]any)["configMap"] = map[string]any{"name": "database-backup-healthcheck", "key": "BACKUP_PING_URL"}
	})
	h.pass()

	held := h.heldConfigMap("database-backup-healthcheck")
	mustMatchMap(t, held.Data, map[string]string{"BACKUP_PING_URL": h.check("example", "database-backup").Status.PingURL})
}

// writes counts the requests after the first skip that write to the
// fake API server.
func (h *harness) writes(skip int) int {
	count := 0
	for _, request := range h.api.requests()[skip:] {
		if request.Method != http.MethodGet {
			count++
		}
	}
	return count
}

func TestASteadyPingCheckWritesNothing(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, pingCheck(backupConfigMap("database-backup-healthcheck", "")))
	h.pass()
	h.pass()
	calls, requests := h.calls.Load(), len(h.api.requests())

	h.pass()

	mustMatch(t, h.calls.Load(), calls)
	mustMatch(t, h.writes(requests), 0)
}

// The watch's event for the operator's own write may reach the store
// after the next pass, and that pass does not write the same thing
// again. No watch runs here: a list fills the stores, and a second
// list brings the Check's store up to date and not the ConfigMaps'.
func TestAPingCheckWritesItsConfigMapOnce(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, pingCheck(backupConfigMap("database-backup-healthcheck", "")))
	_, err := h.c.list(t.Context())
	mustSucceed(t, err)
	h.c.pass(t.Context())
	_, err = h.c.watches[checksResource].list(t.Context())
	mustSucceed(t, err)

	h.c.pass(t.Context())

	mustMatch(t, h.api.requestCount("PATCH", "/api/v1/namespaces/example/configmaps/database-backup-healthcheck", false), 1)
}

func TestARenamedConfigMapDeletesTheOldOne(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, pingCheck(backupConfigMap("database-backup-healthcheck", "")))
	h.pass()

	h.editCheck("example", "database-backup", func(spec map[string]any) {
		spec["ping"].(map[string]any)["configMap"] = map[string]any{"name": "backup-ping"}
	})
	h.pass()

	check := h.check("example", "database-backup")
	mustMatch(t, h.api.read(configMapsResource, "example", "database-backup-healthcheck", &configMap{}), false)
	mustMatchMap(t, h.heldConfigMap("backup-ping").Data, map[string]string{"HEALTHCHECK_URL": check.Status.PingURL})
	mustMatch(t, check.Status.ConfigMap, "backup-ping")
}

// A Check that stops naming a ConfigMap, by dropping configMap or by
// changing its kind, deletes the one it wrote.
func TestACheckThatNamesNoConfigMapDeletesTheOldOne(t *testing.T) {
	cases := []struct {
		name   string
		change func(spec map[string]any)
	}{
		{"no configMap", func(spec map[string]any) {
			delete(spec["ping"].(map[string]any), "configMap")
		}},
		{"an http check", func(spec map[string]any) {
			delete(spec, "ping")
			spec["http"] = map[string]any{"interval": "5m", "requests": []any{map[string]any{"url": "https://example.com/"}}}
		}},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			h := startHarness(t)
			h.api.create(checksResource, pingCheck(backupConfigMap("database-backup-healthcheck", "")))
			h.pass()

			h.editCheck("example", "database-backup", one.change)
			h.pass()

			mustMatch(t, h.api.read(configMapsResource, "example", "database-backup-healthcheck", &configMap{}), false)
			mustMatch(t, h.check("example", "database-backup").Status.ConfigMap, "")
		})
	}
}

func TestAPingCheckWithoutAConfigMapWritesNone(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, pingCheck(PingProbe{Timeout: "24h"}))

	h.pass()

	mustMatch(t, len(h.api.versions(configMapsResource, "")), 0)
	for _, request := range h.api.requests() {
		mustMatch(t, strings.Contains(request.Path, "/configmaps/"), false)
	}
	mustMatch(t, h.check("example", "database-backup").Status.ConfigMap, "")
}

// Only a probe or a reported run sets Passing, so a Check that becomes
// a ping check drops the Passing its probe left.
func TestACheckThatBecomesAPingCheckLosesPassing(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, httpCheck("example", "website", startSite(t, http.StatusOK)))
	h.runProbes()
	h.pass()
	h.probedCheck("example", "website")

	h.editCheck("example", "website", func(spec map[string]any) {
		delete(spec, "http")
		spec["ping"] = map[string]any{"timeout": "24h"}
	})
	h.pass()

	check := h.check("example", "website")
	mustMatch(t, conditionOf(check, "Passing").Status, "")
	mustMatch(t, conditionOf(check, "Ready").Status, ConditionTrue)
}
