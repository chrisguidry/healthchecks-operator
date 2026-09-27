package main

import (
	"encoding/json"
	"testing"
)

func TestCheckSpecProbeKind(t *testing.T) {
	cases := []struct {
		name string
		spec CheckSpec
		want ProbeKind
	}{
		{"no probe", CheckSpec{}, ProbeKindNone},
		{"http", CheckSpec{HTTP: &HTTPProbe{}}, ProbeKindHTTP},
		{"tls", CheckSpec{TLS: &TLSProbe{}}, ProbeKindTLS},
		{"cronJob", CheckSpec{CronJob: &CronJobProbe{}}, ProbeKindCronJob},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mustMatch(t, c.spec.ProbeKind(), c.want)
		})
	}
}

func TestProbeKindString(t *testing.T) {
	cases := []struct {
		kind ProbeKind
		want string
	}{
		{ProbeKindNone, "none"},
		{ProbeKindHTTP, "http"},
		{ProbeKindTLS, "tls"},
		{ProbeKindCronJob, "cronJob"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			mustMatch(t, c.kind.String(), c.want)
		})
	}
}

func TestCheckEffectiveSlug(t *testing.T) {
	cases := []struct {
		name string
		spec CheckSpec
		meta ObjectMeta
		want string
	}{
		{"defaults from namespace and name", CheckSpec{}, ObjectMeta{Namespace: "example", Name: "website"}, "example-website"},
		{"an explicit slug wins", CheckSpec{Slug: "example-website"}, ObjectMeta{Namespace: "example", Name: "site"}, "example-website"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			check := Check{Metadata: c.meta, Spec: c.spec}
			mustMatch(t, check.EffectiveSlug(), c.want)
		})
	}
}

func TestCheckEffectiveDisplayName(t *testing.T) {
	cases := []struct {
		name string
		spec CheckSpec
		meta ObjectMeta
		want string
	}{
		{"defaults from namespace and name", CheckSpec{}, ObjectMeta{Namespace: "example", Name: "website"}, "example/website"},
		{"an explicit display name wins", CheckSpec{DisplayName: "example.com"}, ObjectMeta{Namespace: "example", Name: "website"}, "example.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			check := Check{Metadata: c.meta, Spec: c.spec}
			mustMatch(t, check.EffectiveDisplayName(), c.want)
		})
	}
}

// A Go-marshalled CheckStatus has to validate against the schema the
// same JSON tags describe, so a status the operator writes is never a
// status the API server refuses.
func TestCheckStatusValidatesAgainstTheSchema(t *testing.T) {
	status := CheckStatus{
		Slug:    "example-website",
		UUID:    "3ce96f5a-8bb0-4e69-8e9c-1a5b2a9d0000",
		PingURL: "https://healthchecks.example.com/ping/3ce96f5a-8bb0-4e69-8e9c-1a5b2a9d0000",
		Conditions: []Condition{
			{Type: "Ready", Status: ConditionTrue, ObservedGeneration: 3, LastTransitionTime: "2026-09-27T11:00:00Z"},
			{Type: "Passing", Status: ConditionFalse, Reason: "ProbeFailed", Message: "https://example.com/: status 500, want 200", LastTransitionTime: "2026-09-27T12:00:00Z"},
		},
	}
	raw, err := json.Marshal(status)
	mustSucceed(t, err)

	var decoded map[string]any
	mustSucceed(t, json.Unmarshal(raw, &decoded))

	object := check(httpSpec())
	object["status"] = decoded

	if errs := validateObject(t, checksCRD, object); len(errs) > 0 {
		t.Errorf("a Go-marshalled status failed validation: %v", errs)
	}
}
