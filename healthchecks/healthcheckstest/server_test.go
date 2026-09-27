package healthcheckstest_test

import (
	"net/http"
	"testing"

	"github.com/chrisguidry/healthchecks-operator/healthchecks/healthcheckstest"
)

// These tests call the fake server directly with net/http, rather than
// through a healthchecks.Client, to prove the fake's own HTTP behavior
// independent of the client that the rest of this package's tests use
// to exercise it.

func TestMissingAPIKeyIsUnauthorized(t *testing.T) {
	server := healthcheckstest.NewServer("the-key")
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL() + "/api/v3/channels/")
	if err != nil {
		t.Fatalf("GET /api/v3/channels/: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestDeletingAnUnknownUUIDIs404(t *testing.T) {
	server := healthcheckstest.NewServer("the-key")
	t.Cleanup(server.Close)

	req, _ := http.NewRequest(http.MethodDelete, server.URL()+"/api/v3/checks/no-such-uuid", nil)
	req.Header.Set("X-Api-Key", "the-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestPingingAnUnknownUUIDIs404(t *testing.T) {
	server := healthcheckstest.NewServer("the-key")
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL() + "/ping/no-such-uuid")
	if err != nil {
		t.Fatalf("GET ping url: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestSeedChannelIsReadableBeforeAnyCheckExists(t *testing.T) {
	server := healthcheckstest.NewServer("the-key")
	t.Cleanup(server.Close)

	id := server.SeedChannel("Pushover", "po")
	if id == "" {
		t.Fatal("SeedChannel returned an empty id")
	}

	req, _ := http.NewRequest(http.MethodGet, server.URL()+"/api/v3/channels/", nil)
	req.Header.Set("X-Api-Key", "the-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v3/channels/: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestCheckReportsFalseForAnUnknownSlug(t *testing.T) {
	server := healthcheckstest.NewServer("the-key")
	t.Cleanup(server.Close)

	if _, ok := server.Check("never-created"); ok {
		t.Error("Check(\"never-created\") = true, want false")
	}
}

func TestPingsReportsEmptyForAnUnknownUUID(t *testing.T) {
	server := healthcheckstest.NewServer("the-key")
	t.Cleanup(server.Close)

	if pings := server.Pings("no-such-uuid"); len(pings) != 0 {
		t.Errorf("Pings(\"no-such-uuid\") = %+v, want empty", pings)
	}
}
