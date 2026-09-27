package main

import "testing"

// check builds an object the cluster accepts, so each case below states
// the one thing it changes.
func check(spec map[string]any) map[string]any {
	return map[string]any{
		"apiVersion": "healthchecks.guid.foo/v1alpha1",
		"kind":       "Check",
		"metadata":   map[string]any{"name": "website", "namespace": "example"},
		"spec":       spec,
	}
}

func projectRef() map[string]any {
	return map[string]any{"kind": "ClusterProject", "name": "internal"}
}

func httpSpec() map[string]any {
	return map[string]any{
		"projectRef": projectRef(),
		"http": map[string]any{
			"interval": "5m",
			"requests": []any{
				map[string]any{"url": "https://example.com/"},
			},
		},
	}
}

func tlsSpec() map[string]any {
	return map[string]any{
		"projectRef": projectRef(),
		"tls": map[string]any{
			"interval":     "24h",
			"host":         "example.com:443",
			"minRemaining": "336h",
		},
	}
}

func cronJobSpec() map[string]any {
	return map[string]any{
		"projectRef": projectRef(),
		"cronJob":    map[string]any{"name": "database-backup"},
	}
}

func TestCheckCRDValidatesExamples(t *testing.T) {
	cases := []struct {
		name    string
		object  map[string]any
		wantErr bool
	}{
		{name: "an http check", object: check(httpSpec())},
		{name: "a tls check", object: check(tlsSpec())},
		{name: "a cronJob check", object: check(cronJobSpec())},
		{
			name: "a request with a literal header",
			object: check(map[string]any{
				"projectRef": projectRef(),
				"http": map[string]any{
					"interval": "5m",
					"requests": []any{
						map[string]any{
							"url": "https://example.com/",
							"headers": []any{
								map[string]any{"name": "Host", "value": "example.com"},
							},
						},
					},
				},
			}),
		},
		{
			name: "a request with a header from a secret",
			object: check(map[string]any{
				"projectRef": projectRef(),
				"http": map[string]any{
					"interval": "5m",
					"requests": []any{
						map[string]any{
							"url": "https://example.com/",
							"headers": []any{
								map[string]any{
									"name":      "Authorization",
									"valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "probe-token", "key": "header"}},
								},
							},
						},
					},
				},
			}),
		},
		{
			name:    "no projectRef",
			object:  check(map[string]any{"http": map[string]any{"interval": "5m", "requests": []any{map[string]any{"url": "https://example.com/"}}}}),
			wantErr: true,
		},
		{
			name: "a projectRef of the wrong kind",
			object: check(map[string]any{
				"projectRef": map[string]any{"kind": "Project", "name": "internal"},
				"http":       map[string]any{"interval": "5m", "requests": []any{map[string]any{"url": "https://example.com/"}}},
			}),
			wantErr: true,
		},
		{
			name:    "no probe",
			object:  check(map[string]any{"projectRef": projectRef()}),
			wantErr: true,
		},
		{
			name: "two probes",
			object: check(map[string]any{
				"projectRef": projectRef(),
				"http":       map[string]any{"interval": "5m", "requests": []any{map[string]any{"url": "https://example.com/"}}},
				"tls":        map[string]any{"interval": "24h", "host": "example.com:443", "minRemaining": "336h"},
			}),
			wantErr: true,
		},
		{
			name: "an http interval under a minute",
			object: check(map[string]any{
				"projectRef": projectRef(),
				"http":       map[string]any{"interval": "30s", "requests": []any{map[string]any{"url": "https://example.com/"}}},
			}),
			wantErr: true,
		},
		{
			name: "an http interval of exactly a minute",
			object: check(map[string]any{
				"projectRef": projectRef(),
				"http":       map[string]any{"interval": "1m", "requests": []any{map[string]any{"url": "https://example.com/"}}},
			}),
		},
		{
			name: "an http probe with no requests",
			object: check(map[string]any{
				"projectRef": projectRef(),
				"http":       map[string]any{"interval": "5m", "requests": []any{}},
			}),
			wantErr: true,
		},
		{
			name: "a header with no value and no valueFrom",
			object: check(map[string]any{
				"projectRef": projectRef(),
				"http": map[string]any{
					"interval": "5m",
					"requests": []any{
						map[string]any{
							"url":     "https://example.com/",
							"headers": []any{map[string]any{"name": "Host"}},
						},
					},
				},
			}),
			wantErr: true,
		},
		{
			name: "a header with both value and valueFrom",
			object: check(map[string]any{
				"projectRef": projectRef(),
				"http": map[string]any{
					"interval": "5m",
					"requests": []any{
						map[string]any{
							"url": "https://example.com/",
							"headers": []any{
								map[string]any{
									"name":      "Host",
									"value":     "example.com",
									"valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "probe-token", "key": "header"}},
								},
							},
						},
					},
				},
			}),
			wantErr: true,
		},
		{
			name: "a tls interval under a minute",
			object: check(map[string]any{
				"projectRef": projectRef(),
				"tls":        map[string]any{"interval": "30s", "host": "example.com:443", "minRemaining": "336h"},
			}),
			wantErr: true,
		},
		{
			name: "a tls minRemaining of zero",
			object: check(map[string]any{
				"projectRef": projectRef(),
				"tls":        map[string]any{"interval": "24h", "host": "example.com:443", "minRemaining": "0s"},
			}),
			wantErr: true,
		},
		{
			name: "a tls minRemaining that is negative",
			object: check(map[string]any{
				"projectRef": projectRef(),
				"tls":        map[string]any{"interval": "24h", "host": "example.com:443", "minRemaining": "-1h"},
			}),
			wantErr: true,
		},
		{
			name: "a grace period in Go duration format",
			object: check(map[string]any{
				"projectRef": projectRef(),
				"grace":      "15m",
				"http":       map[string]any{"interval": "5m", "requests": []any{map[string]any{"url": "https://example.com/"}}},
			}),
		},
		{
			name: "a grace period that is not a duration",
			object: check(map[string]any{
				"projectRef": projectRef(),
				"grace":      "fortnight",
				"http":       map[string]any{"interval": "5m", "requests": []any{map[string]any{"url": "https://example.com/"}}},
			}),
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := validateObject(t, checksCRD, c.object)
			if got := len(errs) > 0; got != c.wantErr {
				t.Errorf("got errors %v, want an error: %v", errs, c.wantErr)
			}
		})
	}
}

// A Check with no spec never reaches the one-of rule, because CEL runs
// only where the field it is written on exists. The root required list
// is what refuses it.
func TestCRDRefusesACheckWithNoSpec(t *testing.T) {
	errs := validateObject(t, checksCRD, map[string]any{
		"apiVersion": "healthchecks.guid.foo/v1alpha1",
		"kind":       "Check",
		"metadata":   map[string]any{"name": "website", "namespace": "example"},
	})
	if len(errs) == 0 {
		t.Error("a Check with no spec passed validation")
	}
}
