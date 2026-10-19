package main

import "testing"

// clusterProject builds an object the cluster accepts, so each case
// below states the one thing it changes.
func clusterProject(spec map[string]any) map[string]any {
	return map[string]any{
		"apiVersion": "healthchecks.guid.foo/v1alpha1",
		"kind":       "ClusterProject",
		"metadata":   map[string]any{"name": "internal"},
		"spec":       spec,
	}
}

func clusterProjectSpec() map[string]any {
	return map[string]any{
		"url":          "https://healthchecks.example.com",
		"apiKeySecret": map[string]any{"name": "internal-healthchecks", "key": "api-key"},
	}
}

func TestClusterProjectCRDValidatesExamples(t *testing.T) {
	cases := []struct {
		name    string
		object  map[string]any
		wantErr bool
	}{
		{
			name:   "a project with a URL and a key",
			object: clusterProject(clusterProjectSpec()),
		},
		{
			name: "a project with default channels",
			object: clusterProject(map[string]any{
				"url":          "https://healthchecks.example.com",
				"apiKeySecret": map[string]any{"name": "internal-healthchecks", "key": "api-key"},
				"channels":     []any{"Pushover"},
			}),
		},
		{
			name:    "no url",
			object:  clusterProject(map[string]any{"apiKeySecret": map[string]any{"name": "internal-healthchecks", "key": "api-key"}}),
			wantErr: true,
		},
		{
			name:    "a url with no scheme",
			object:  clusterProject(map[string]any{"url": "healthchecks.example.com", "apiKeySecret": map[string]any{"name": "internal-healthchecks", "key": "api-key"}}),
			wantErr: true,
		},
		{
			name:    "no apiKeySecret",
			object:  clusterProject(map[string]any{"url": "https://healthchecks.example.com"}),
			wantErr: true,
		},
		{
			name:    "an apiKeySecret with no key",
			object:  clusterProject(map[string]any{"url": "https://healthchecks.example.com", "apiKeySecret": map[string]any{"name": "internal-healthchecks"}}),
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := validateObject(t, clusterProjectsCRD, c.object)
			if got := len(errs) > 0; got != c.wantErr {
				t.Errorf("got errors %v, want an error: %v", errs, c.wantErr)
			}
		})
	}
}

// projectWithHold is a valid project whose hold has one field changed.
func projectWithHold(change func(hold, while map[string]any)) map[string]any {
	while := map[string]any{
		"apiVersion": "example.com/v1",
		"resource":   "rollouts",
		"name":       "fleet",
		"condition":  map[string]any{"type": "Progressing", "status": "True", "reason": "RollingOut"},
	}
	hold := map[string]any{"while": while, "maxDuration": "3h"}
	change(hold, while)
	spec := clusterProjectSpec()
	spec["hold"] = hold
	return clusterProject(spec)
}

func TestClusterProjectCRDValidatesHolds(t *testing.T) {
	cases := []struct {
		name    string
		object  map[string]any
		wantErr bool
	}{
		{"a hold on a cluster-scoped object", projectWithHold(func(hold, while map[string]any) {}), false},
		{"a hold on a namespaced object", projectWithHold(func(hold, while map[string]any) { while["namespace"] = "example" }), false},
		{"a hold on a core object", projectWithHold(func(hold, while map[string]any) { while["apiVersion"] = "v1" }), false},
		{"a condition with any reason", projectWithHold(func(hold, while map[string]any) {
			while["condition"] = map[string]any{"type": "Progressing", "status": "True"}
		}), false},
		{"no maxDuration", projectWithHold(func(hold, while map[string]any) { delete(hold, "maxDuration") }), true},
		{"a maxDuration with no unit", projectWithHold(func(hold, while map[string]any) { hold["maxDuration"] = "3" }), true},
		{"no resource", projectWithHold(func(hold, while map[string]any) { delete(while, "resource") }), true},
		{"a resource named by its kind", projectWithHold(func(hold, while map[string]any) { while["resource"] = "Rollout" }), true},
		{"no name", projectWithHold(func(hold, while map[string]any) { delete(while, "name") }), true},
		{"a status that is not a condition status", projectWithHold(func(hold, while map[string]any) {
			while["condition"] = map[string]any{"type": "Progressing", "status": "yes"}
		}), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := validateObject(t, clusterProjectsCRD, c.object)
			if got := len(errs) > 0; got != c.wantErr {
				t.Errorf("got errors %v, want an error: %v", errs, c.wantErr)
			}
		})
	}
}
