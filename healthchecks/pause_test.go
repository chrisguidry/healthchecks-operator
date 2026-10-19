package healthchecks_test

import (
	"context"
	"testing"
	"time"

	"github.com/chrisguidry/healthchecks-operator/healthchecks"
)

// upsertWebsite creates the check "website" with manualResume, and
// returns what Upsert answered.
func upsertWebsite(t *testing.T, client *healthchecks.Client, manualResume bool) *healthchecks.UpsertResult {
	t.Helper()
	result, err := client.Upsert(context.Background(), healthchecks.UpsertRequest{
		Slug:         "website",
		Grace:        time.Minute,
		Period:       healthchecks.FixedTimeout(time.Hour),
		ManualResume: manualResume,
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	return result
}

func TestUpsertSendsManualResume(t *testing.T) {
	cases := []struct {
		name         string
		manualResume bool
	}{
		{"set", true},
		{"cleared", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, sent := captureUpsert(t, healthchecks.UpsertRequest{
				Slug:         "website",
				Period:       healthchecks.FixedTimeout(time.Hour),
				ManualResume: tc.manualResume,
			})
			if sent["manual_resume"] != tc.manualResume {
				t.Errorf("manual_resume = %v, want %v", sent["manual_resume"], tc.manualResume)
			}
		})
	}
}

func TestUpsertReportsTheCheckStatus(t *testing.T) {
	client, server := newTestClient(t)
	created := upsertWebsite(t, client, false)
	if created.Status != "new" {
		t.Errorf("a new check's status = %q, want new", created.Status)
	}

	server.SetStatus("website", "down")
	updated := upsertWebsite(t, client, false)
	if updated.Status != "down" {
		t.Errorf("status after an update = %q, want down", updated.Status)
	}
}

func TestPausePausesTheCheck(t *testing.T) {
	client, server := newTestClient(t)
	result := upsertWebsite(t, client, true)

	if err := client.Pause(context.Background(), result.UUID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	stored, _ := server.Check("website")
	if stored.Status != "paused" {
		t.Errorf("status = %q, want paused", stored.Status)
	}
}

func TestPauseOfAnUnknownCheckIsAnError(t *testing.T) {
	client, _ := newTestClient(t)
	if err := client.Pause(context.Background(), "00000000-0000-4000-8000-000000000000"); err == nil {
		t.Error("Pause of a UUID the server never saw returned no error")
	}
}
