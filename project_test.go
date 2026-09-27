package main

import (
	"testing"
)

func TestAProjectResolvesItsChannels(t *testing.T) {
	h := startHarness(t)

	h.pass()

	project := h.project("internal")
	mustDeepEqual(t, project.Status.Channels, []ResolvedChannel{{Name: "Pushover", ID: h.pushover}})
	ready, _ := findCondition(project.Status.Conditions, "Ready")
	mustMatch(t, ready, Condition{
		Type:               "Ready",
		Status:             ConditionTrue,
		ObservedGeneration: 1,
		Reason:             "Connected",
		Message:            "the API key works, and every channel resolves",
		LastTransitionTime: "2026-09-27T12:00:00Z",
	})
}

func TestAProjectNamesWhatIsWrong(t *testing.T) {
	cases := []struct {
		name    string
		secret  map[string][]byte
		channel string
		reason  string
		message string
	}{
		{
			"a Secret with no key",
			map[string][]byte{"token": []byte(projectKey)},
			"Pushover",
			"SecretUnreadable",
			`Secret healthchecks-operator/internal-healthchecks has no key "api-key"`,
		},
		{
			"a key Healthchecks refuses",
			map[string][]byte{"api-key": []byte("wrong")},
			"Pushover",
			"HealthchecksError",
			`listing channels: healthchecks: Unauthorized: {"error":"wrong api key"}`,
		},
		{
			"a channel that does not exist",
			map[string][]byte{"api-key": []byte(projectKey)},
			"Slack",
			"ChannelNotFound",
			`no channel in the project is named "Slack"`,
		},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			h := startHarness(t)
			h.api.update(secretsResource, operatorNamespace, "internal-healthchecks", func(object map[string]any) {
				object["data"] = toObject(t, secretWith("", "", one.secret))["data"]
			})
			h.api.update(clusterProjectsResource, "", "internal", func(object map[string]any) {
				object["spec"].(map[string]any)["channels"] = []any{one.channel}
			})

			h.pass()

			ready, _ := findCondition(h.project("internal").Status.Conditions, "Ready")
			mustMatch(t, ready.Status, ConditionFalse)
			mustMatch(t, ready.Reason, one.reason)
			mustMatch(t, ready.Message, one.message)
			mustMatch(t, h.log.lines()[0], "clusterproject internal: "+one.message)
		})
	}
}

// A person may make a missing channel after the operator looked, so a
// project that names one lists the channels again on the next pass.
func TestAProjectFindsAChannelMadeLater(t *testing.T) {
	h := startHarness(t)
	h.api.update(clusterProjectsResource, "", "internal", func(object map[string]any) {
		object["spec"].(map[string]any)["channels"] = []any{"Slack"}
	})
	h.pass()
	slack := h.hc.SeedChannel("Slack", "slack")

	h.pass()

	mustDeepEqual(t, h.project("internal").Status.Channels, []ResolvedChannel{{Name: "Slack", ID: slack}})
}
