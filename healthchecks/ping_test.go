package healthchecks_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chrisguidry/healthchecks-operator/healthchecks"
)

func TestPingSuffixesForEachKind(t *testing.T) {
	cases := []struct {
		kind healthchecks.PingKind
		path string
	}{
		{healthchecks.PingSuccess, "/check-uuid"},
		{healthchecks.PingStart, "/check-uuid/start"},
		{healthchecks.PingFail, "/check-uuid/fail"},
	}

	for _, c := range cases {
		var gotPath string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
		}))

		err := healthchecks.Ping(context.Background(), nil, server.URL+"/check-uuid", c.kind, "")
		server.Close()

		if err != nil {
			t.Fatalf("Ping(%v): %v", c.kind, err)
		}
		if gotPath != c.path {
			t.Errorf("Ping(%v) path = %q, want %q", c.kind, gotPath, c.path)
		}
	}
}

func TestPingSendsBodyAsTextPlain(t *testing.T) {
	var gotBody, gotContentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		gotBody = string(data)
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	err := healthchecks.Ping(context.Background(), nil, server.URL, healthchecks.PingFail, "status 502, want 200 or 401")
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if gotBody != "status 502, want 200 or 401" {
		t.Errorf("body = %q, want the failure reason verbatim", gotBody)
	}
	if gotContentType != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain", gotContentType)
	}
}

func TestPingCapsTheBodyAt100KB(t *testing.T) {
	var gotLength int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		gotLength = len(data)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	oversized := strings.Repeat("x", 200_000)
	if err := healthchecks.Ping(context.Background(), nil, server.URL, healthchecks.PingSuccess, oversized); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if gotLength != 100_000 {
		t.Errorf("body length = %d, want 100000", gotLength)
	}
}

func TestPingReturnsAPIErrorOnFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}))
	t.Cleanup(server.Close)

	err := healthchecks.Ping(context.Background(), nil, server.URL, healthchecks.PingSuccess, "")
	var apiErr *healthchecks.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *healthchecks.APIError", err)
	}
	if apiErr.StatusCode != http.StatusNotFound || apiErr.Body != "not found" {
		t.Errorf("apiErr = %+v, want 404 %q", apiErr, "not found")
	}
}

func TestPingAgainstTheFakeServerIsRecorded(t *testing.T) {
	client, server := newTestClient(t)
	ctx := context.Background()

	result, err := client.Upsert(ctx, healthchecks.UpsertRequest{
		Slug:   "website",
		Grace:  time.Minute,
		Period: healthchecks.FixedTimeout(time.Minute),
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := healthchecks.Ping(ctx, nil, result.PingURL, healthchecks.PingStart, ""); err != nil {
		t.Fatalf("Ping start: %v", err)
	}
	if err := healthchecks.Ping(ctx, nil, result.PingURL, healthchecks.PingSuccess, "all good"); err != nil {
		t.Fatalf("Ping success: %v", err)
	}

	pings := server.Pings(result.UUID)
	if len(pings) != 2 {
		t.Fatalf("recorded %d pings, want 2", len(pings))
	}
	if pings[0].Kind != "start" || pings[1].Kind != "success" || pings[1].Body != "all good" {
		t.Errorf("pings = %+v, want [start, success with body %q]", pings, "all good")
	}
}
