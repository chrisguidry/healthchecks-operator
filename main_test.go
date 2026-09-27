package main

import "testing"

// environment is a getenv over a fixed set of variables.
func environment(variables map[string]string) func(string) string {
	return func(name string) string { return variables[name] }
}

func TestReadSettingsTakesTheEnvironment(t *testing.T) {
	config, err := readSettings(environment(map[string]string{
		"OPERATOR_NAMESPACE": "healthchecks-operator",
		"HEARTBEAT_PROJECT":  "outside",
		"HEARTBEAT_SLUG":     "operator-heartbeat",
		"METRICS_PORT":       "9300",
	}))

	mustSucceed(t, err)
	mustMatch(t, config, settings{
		namespace:        "healthchecks-operator",
		heartbeatProject: "outside",
		heartbeatSlug:    "operator-heartbeat",
		metricsPort:      9300,
	})
}

func TestReadSettingsDefaultsTheMetricsPortAndTheHeartbeat(t *testing.T) {
	config, err := readSettings(environment(map[string]string{"OPERATOR_NAMESPACE": "healthchecks-operator"}))

	mustSucceed(t, err)
	mustMatch(t, config, settings{namespace: "healthchecks-operator", metricsPort: 9200})
}

func TestReadSettingsRefusesAnIncompleteEnvironment(t *testing.T) {
	cases := []struct {
		name      string
		variables map[string]string
	}{
		{"no namespace", map[string]string{}},
		{"a project with no slug", map[string]string{"OPERATOR_NAMESPACE": "hc", "HEARTBEAT_PROJECT": "outside"}},
		{"a slug with no project", map[string]string{"OPERATOR_NAMESPACE": "hc", "HEARTBEAT_SLUG": "operator"}},
		{"a port that is not a number", map[string]string{"OPERATOR_NAMESPACE": "hc", "METRICS_PORT": "metrics"}},
		{"a port out of range", map[string]string{"OPERATOR_NAMESPACE": "hc", "METRICS_PORT": "70000"}},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			_, err := readSettings(environment(one.variables))

			mustMatch(t, err != nil, true)
		})
	}
}

func TestRunStopsOnABadEnvironment(t *testing.T) {
	cases := []struct {
		name      string
		namespace string
		host      string
	}{
		{"no namespace", "", "10.0.0.1"},
		{"not in a cluster", "healthchecks-operator", ""},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			t.Setenv("OPERATOR_NAMESPACE", one.namespace)
			t.Setenv("KUBERNETES_SERVICE_HOST", one.host)

			mustMatch(t, run() != nil, true)
		})
	}
}
