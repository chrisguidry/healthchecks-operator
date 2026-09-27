package healthchecks_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/chrisguidry/healthchecks-operator/healthchecks"
	"github.com/chrisguidry/healthchecks-operator/healthchecks/healthcheckstest"
)

// newTestClient starts a fake server seeded with the given API key and
// returns a Client pointed at it, closing the server when the test ends.
// Every test file in this package that needs a live Client uses it.
func newTestClient(t *testing.T) (*healthchecks.Client, *healthcheckstest.Server) {
	t.Helper()
	server := healthcheckstest.NewServer("test-api-key")
	t.Cleanup(server.Close)
	client := healthchecks.NewClient(server.URL(), "test-api-key", nil)
	return client, server
}

func TestAWrongAPIKeyIsUnauthorized(t *testing.T) {
	_, server := newTestClient(t)
	client := healthchecks.NewClient(server.URL(), "wrong-key", nil)

	_, err := client.Upsert(context.Background(), healthchecks.UpsertRequest{
		Slug:   "backup",
		Grace:  time.Minute,
		Period: healthchecks.FixedTimeout(time.Hour),
	})

	var apiErr *healthchecks.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *healthchecks.APIError", err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want 401", apiErr.StatusCode)
	}
}
