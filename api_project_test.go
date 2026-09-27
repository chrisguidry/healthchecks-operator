package main

import (
	"encoding/json"
	"testing"
)

// A Go-marshalled ClusterProjectStatus has to validate against the
// schema the same JSON tags describe, so a status the operator writes
// is never a status the API server refuses.
func TestClusterProjectStatusValidatesAgainstTheSchema(t *testing.T) {
	status := ClusterProjectStatus{
		Channels: []ResolvedChannel{
			{Name: "Pushover", ID: "c1b1f2f0-1234-4a5b-9c8d-0000000000aa"},
		},
		Conditions: []Condition{
			{Type: "Ready", Status: ConditionTrue, ObservedGeneration: 1, LastTransitionTime: "2026-09-27T11:00:00Z"},
		},
	}
	raw, err := json.Marshal(status)
	mustSucceed(t, err)

	var decoded map[string]any
	mustSucceed(t, json.Unmarshal(raw, &decoded))

	object := clusterProject(clusterProjectSpec())
	object["status"] = decoded

	if errs := validateObject(t, clusterProjectsCRD, object); len(errs) > 0 {
		t.Errorf("a Go-marshalled status failed validation: %v", errs)
	}
}
