package healthchecks_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chrisguidry/healthchecks-operator/healthchecks"
)

// captureUpsert runs one Upsert against a plain httptest handler that
// records the request and answers with a fixed body, so a test can
// assert on the exact bytes a Client sent rather than on the fake
// server's own parsing of them.
func captureUpsert(t *testing.T, req healthchecks.UpsertRequest) (*http.Request, map[string]any) {
	t.Helper()

	var (
		captured *http.Request
		body     []byte
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Clone(r.Context())
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"uuid":"test-uuid","slug":"test-slug","ping_url":"https://hc-ping.com/test-uuid"}`))
	}))
	t.Cleanup(server.Close)

	client := healthchecks.NewClient(server.URL, "test-key", nil)
	if _, err := client.Upsert(context.Background(), req); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("request body was not JSON: %v (%q)", err, body)
	}
	return captured, sent
}

func TestUpsertSendsTagsAsOneSpaceSeparatedString(t *testing.T) {
	_, sent := captureUpsert(t, healthchecks.UpsertRequest{
		Slug:   "website",
		Tags:   []string{"internet", "website"},
		Grace:  time.Minute,
		Period: healthchecks.FixedTimeout(5 * time.Minute),
	})

	if sent["tags"] != "internet website" {
		t.Errorf("tags = %#v, want %q", sent["tags"], "internet website")
	}
}

func TestUpsertSendsChannelsAsCommaSeparatedIDs(t *testing.T) {
	_, sent := captureUpsert(t, healthchecks.UpsertRequest{
		Slug:     "website",
		Channels: []string{"chan-1", "chan-2"},
		Grace:    time.Minute,
		Period:   healthchecks.FixedTimeout(5 * time.Minute),
	})

	if sent["channels"] != "chan-1,chan-2" {
		t.Errorf("channels = %#v, want %q", sent["channels"], "chan-1,chan-2")
	}
}

func TestUpsertOmitsScheduleWhenPeriodIsATimeout(t *testing.T) {
	_, sent := captureUpsert(t, healthchecks.UpsertRequest{
		Slug:   "website",
		Grace:  time.Minute,
		Period: healthchecks.FixedTimeout(5 * time.Minute),
	})

	if _, present := sent["schedule"]; present {
		t.Errorf("schedule = %#v, want absent", sent["schedule"])
	}
	if _, present := sent["tz"]; present {
		t.Errorf("tz = %#v, want absent", sent["tz"])
	}
	if sent["timeout"] != float64(300) {
		t.Errorf("timeout = %#v, want 300", sent["timeout"])
	}
}

func TestUpsertOmitsTimeoutWhenPeriodIsASchedule(t *testing.T) {
	_, sent := captureUpsert(t, healthchecks.UpsertRequest{
		Slug:   "backup",
		Grace:  time.Hour,
		Period: healthchecks.CronSchedule("0 3 * * *", "America/New_York"),
	})

	if _, present := sent["timeout"]; present {
		t.Errorf("timeout = %#v, want absent", sent["timeout"])
	}
	if sent["schedule"] != "0 3 * * *" {
		t.Errorf("schedule = %#v, want %q", sent["schedule"], "0 3 * * *")
	}
	if sent["tz"] != "America/New_York" {
		t.Errorf("tz = %#v, want %q", sent["tz"], "America/New_York")
	}
}

func TestUpsertAlwaysSendsUniqueSlug(t *testing.T) {
	_, sent := captureUpsert(t, healthchecks.UpsertRequest{
		Slug:   "website",
		Grace:  time.Minute,
		Period: healthchecks.FixedTimeout(5 * time.Minute),
	})

	unique, ok := sent["unique"].([]any)
	if !ok || len(unique) != 1 || unique[0] != "slug" {
		t.Errorf("unique = %#v, want [\"slug\"]", sent["unique"])
	}
}

func TestUpsertSendsGraceAndNameAndDescEvenWhenEmpty(t *testing.T) {
	_, sent := captureUpsert(t, healthchecks.UpsertRequest{
		Slug:   "website",
		Grace:  90 * time.Second,
		Period: healthchecks.FixedTimeout(5 * time.Minute),
	})

	if sent["name"] != "" {
		t.Errorf("name = %#v, want empty string, not absent", sent["name"])
	}
	if sent["desc"] != "" {
		t.Errorf("desc = %#v, want empty string, not absent", sent["desc"])
	}
	if sent["tags"] != "" {
		t.Errorf("tags = %#v, want empty string, not absent", sent["tags"])
	}
	if sent["channels"] != "" {
		t.Errorf("channels = %#v, want empty string, not absent", sent["channels"])
	}
	if sent["grace"] != float64(90) {
		t.Errorf("grace = %#v, want 90", sent["grace"])
	}
}

func TestUpsertSetsRequestHeaders(t *testing.T) {
	captured, _ := captureUpsert(t, healthchecks.UpsertRequest{
		Slug:   "website",
		Grace:  time.Minute,
		Period: healthchecks.FixedTimeout(5 * time.Minute),
	})

	if got := captured.Header.Get("X-Api-Key"); got != "test-key" {
		t.Errorf("X-Api-Key = %q, want %q", got, "test-key")
	}
	if got := captured.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if captured.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", captured.Method)
	}
	if captured.URL.Path != "/api/v3/checks/" {
		t.Errorf("path = %s, want /api/v3/checks/", captured.URL.Path)
	}
}

func TestUpsertRequiresASlug(t *testing.T) {
	client := healthchecks.NewClient("https://example.com", "test-key", nil)
	_, err := client.Upsert(context.Background(), healthchecks.UpsertRequest{
		Grace:  time.Minute,
		Period: healthchecks.FixedTimeout(5 * time.Minute),
	})
	if err == nil {
		t.Fatal("Upsert with no slug: want an error, got nil")
	}
}

func TestUpsertRequiresAPeriod(t *testing.T) {
	client := healthchecks.NewClient("https://example.com", "test-key", nil)
	_, err := client.Upsert(context.Background(), healthchecks.UpsertRequest{Slug: "website", Grace: time.Minute})
	if err == nil {
		t.Fatal("Upsert with no Period: want an error, got nil")
	}
}

func TestUpsertReturnsAPIErrorWithServerBodyVerbatim(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"json validation error: slug does not match pattern"}`))
	}))
	t.Cleanup(server.Close)

	client := healthchecks.NewClient(server.URL, "test-key", nil)
	_, err := client.Upsert(context.Background(), healthchecks.UpsertRequest{
		Slug:   "Website!",
		Grace:  time.Minute,
		Period: healthchecks.FixedTimeout(5 * time.Minute),
	})

	var apiErr *healthchecks.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *healthchecks.APIError", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want 400", apiErr.StatusCode)
	}
	want := `{"error":"json validation error: slug does not match pattern"}`
	if apiErr.Body != want {
		t.Errorf("Body = %q, want %q", apiErr.Body, want)
	}
}

func TestUpsertCreatesANewCheck(t *testing.T) {
	client, server := newTestClient(t)

	result, err := client.Upsert(context.Background(), healthchecks.UpsertRequest{
		Name:   "example.com",
		Slug:   "example-website",
		Grace:  time.Minute,
		Period: healthchecks.FixedTimeout(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if !result.Created {
		t.Error("Created = false, want true for a new slug")
	}
	if result.UUID == "" || result.PingURL == "" || result.Slug != "example-website" {
		t.Errorf("result = %+v, want a filled-in uuid, ping_url and slug", result)
	}

	stored, ok := server.Check("example-website")
	if !ok {
		t.Fatal("server has no check at example-website")
	}
	if stored.Name != "example.com" {
		t.Errorf("stored name = %q, want example.com", stored.Name)
	}
}

func TestUpsertUpdatesTheExistingCheckAtThatSlug(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()

	first, err := client.Upsert(ctx, healthchecks.UpsertRequest{
		Slug:   "backup",
		Grace:  time.Minute,
		Period: healthchecks.FixedTimeout(time.Hour),
	})
	if err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	second, err := client.Upsert(ctx, healthchecks.UpsertRequest{
		Slug:   "backup",
		Name:   "renamed",
		Grace:  time.Minute,
		Period: healthchecks.FixedTimeout(time.Hour),
	})
	if err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	if second.Created {
		t.Error("Created = true on the second Upsert, want false")
	}
	if second.UUID != first.UUID {
		t.Errorf("second UUID = %s, want the same as first %s", second.UUID, first.UUID)
	}
}

func TestUpsertSwitchesFromScheduleToTimeout(t *testing.T) {
	client, server := newTestClient(t)
	ctx := context.Background()

	_, err := client.Upsert(ctx, healthchecks.UpsertRequest{
		Slug:   "backup",
		Grace:  time.Minute,
		Period: healthchecks.CronSchedule("0 3 * * *", "UTC"),
	})
	if err != nil {
		t.Fatalf("Upsert with a schedule: %v", err)
	}

	_, err = client.Upsert(ctx, healthchecks.UpsertRequest{
		Slug:   "backup",
		Grace:  time.Minute,
		Period: healthchecks.FixedTimeout(time.Hour),
	})
	if err != nil {
		t.Fatalf("Upsert with a timeout: %v", err)
	}

	stored, _ := server.Check("backup")
	if stored.Schedule != "" {
		t.Errorf("Schedule = %q, want cleared after switching to a timeout", stored.Schedule)
	}
	if stored.Timeout != time.Hour {
		t.Errorf("Timeout = %s, want 1h0m0s", stored.Timeout)
	}
}

func TestUpsertSwitchesFromTimeoutToSchedule(t *testing.T) {
	client, server := newTestClient(t)
	ctx := context.Background()

	_, err := client.Upsert(ctx, healthchecks.UpsertRequest{
		Slug:   "backup",
		Grace:  time.Minute,
		Period: healthchecks.FixedTimeout(time.Hour),
	})
	if err != nil {
		t.Fatalf("Upsert with a timeout: %v", err)
	}

	_, err = client.Upsert(ctx, healthchecks.UpsertRequest{
		Slug:   "backup",
		Grace:  time.Minute,
		Period: healthchecks.CronSchedule("0 3 * * *", "UTC"),
	})
	if err != nil {
		t.Fatalf("Upsert with a schedule: %v", err)
	}

	stored, _ := server.Check("backup")
	if stored.Timeout != 0 {
		t.Errorf("Timeout = %s, want cleared after switching to a schedule", stored.Timeout)
	}
	if stored.Schedule != "0 3 * * *" {
		t.Errorf("Schedule = %q, want the cron expression", stored.Schedule)
	}
}
