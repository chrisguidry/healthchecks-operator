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
