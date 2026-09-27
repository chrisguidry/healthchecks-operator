package main

import (
	"strings"
	"testing"
)

func TestHistoryProblem(t *testing.T) {
	cases := []struct {
		name       string
		successful *int32
		failed     *int32
		wantField  string // "" means historyProblem must return ""
	}{
		{"both unset use the Kubernetes defaults", nil, nil, ""},
		{"both explicitly set to 1 or more", int32p(3), int32p(1), ""},
		{"successful set high, failed unset", int32p(5), nil, ""},
		{"successful is 0", int32p(0), nil, "successfulJobsHistoryLimit"},
		{"failed is 0", nil, int32p(0), "failedJobsHistoryLimit"},
		{"both are 0", int32p(0), int32p(0), "successfulJobsHistoryLimit"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cj := batchCronJob{Spec: batchCronJobSpec{
				SuccessfulJobsHistoryLimit: c.successful,
				FailedJobsHistoryLimit:     c.failed,
			}}
			got := historyProblem(cj)
			if c.wantField == "" {
				mustMatch(t, got, "")
				return
			}
			if !strings.Contains(got, c.wantField) {
				t.Errorf("got %q, want a message naming %q", got, c.wantField)
			}
		})
	}
}
