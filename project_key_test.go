package main

import (
	"net/http"
	"testing"
	"time"
)

var apiKeySecretPath = secretsResource.path(operatorNamespace, "internal-healthchecks")

func (h *harness) projectReady() Condition {
	found, _ := findCondition(h.project("internal").Status.Conditions, readyCondition)
	return found
}

func (h *harness) keyReads() int {
	return h.api.requestCount("GET", apiKeySecretPath, false)
}

// The API key is read once, and again only on the key's clock, when
// the ClusterProject's spec changes, or when Healthchecks refuses it.
func TestTheAPIKeyIsReadOnlyWhenItIsDue(t *testing.T) {
	cases := []struct {
		name   string
		change func(h *harness)
		reads  int
	}{
		{"steady passes", func(h *harness) { h.pass(); h.pass() }, 1},
		{"the key's clock", func(h *harness) {
			h.clock.advance(keyReadPeriod)
			h.pass()
		}, 2},
		{"a change to the spec", func(h *harness) {
			h.api.update(clusterProjectsResource, "", "internal", func(object map[string]any) {
				object["spec"].(map[string]any)["channels"] = []any{"Email"}
			})
			h.pass()
		}, 2},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			h := startHarness(t)
			h.pass()

			one.change(h)

			mustMatch(t, h.keyReads(), one.reads)
		})
	}
}

// A read of the key that fails for a reason that may clear, such as an
// API server that does not answer in time, keeps the key that worked,
// so the project and its Checks stay Ready.
func TestAKeyReadThatMayClearKeepsTheKeyThatWorked(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, httpCheck("example", "site", "https://example.com"))
	h.pass()
	h.api.refuse("GET", apiKeySecretPath, http.StatusServiceUnavailable)
	h.clock.advance(keyReadPeriod)

	h.pass()

	mustMatch(t, h.projectReady().Status, ConditionTrue)
	mustMatch(t, conditionOf(h.check("example", "site"), readyCondition).Status, ConditionTrue)
}

// A Secret that is gone is no fault that clears by itself, so the
// project says so.
func TestAKeyWhoseSecretIsGoneMakesTheProjectNotReady(t *testing.T) {
	h := startHarness(t)
	h.pass()
	h.api.delete(secretsResource, operatorNamespace, "internal-healthchecks")
	h.clock.advance(keyReadPeriod)

	h.pass()

	mustMatch(t, h.projectReady().Reason, reasonSecretUnreadable)
}

// A key that Healthchecks refuses is read again on the next pass, so a
// rotated key takes effect without waiting for the key's clock.
func TestARefusedKeyIsReadAgain(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, httpCheck("example", "site", "https://example.com"))
	h.pass()
	h.hc.SetAPIKey("rotated")
	h.api.update(secretsResource, operatorNamespace, "internal-healthchecks", func(object map[string]any) {
		object["data"] = map[string]any{"api-key": []byte("rotated")}
	})
	h.editCheck("example", "site", func(spec map[string]any) { spec["description"] = "Rotated" })
	h.pass()
	// The refused upsert puts the Check on its backoff, which ends
	// before the next pass.
	h.clock.advance(time.Minute)

	h.pass()

	mustMatch(t, h.keyReads(), 2)
	mustMatch(t, conditionOf(h.check("example", "site"), readyCondition).Status, ConditionTrue)
	stored, _ := h.hc.Check(h.check("example", "site").Status.Slug)
	mustMatch(t, stored.Description, "Rotated")
}
