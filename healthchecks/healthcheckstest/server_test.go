package healthcheckstest_test

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
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

// ping sends one ping of kind to the check at slug, as a workload would.
func ping(t *testing.T, server *healthcheckstest.Server, slug, kind string) {
	t.Helper()
	check, _ := server.Check(slug)
	url := check.PingURL
	if kind != "" {
		url += "/" + kind
	}
	resp, err := http.Post(url, "text/plain", nil)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	resp.Body.Close()
}

// upsert creates or updates the check at slug with manual_resume.
func upsert(t *testing.T, server *healthcheckstest.Server, slug string, manualResume bool) {
	t.Helper()
	body := fmt.Sprintf(`{"slug": %q, "timeout": 3600, "manual_resume": %t, "unique": ["slug"]}`, slug, manualResume)
	req, _ := http.NewRequest(http.MethodPost, server.URL()+"/api/v3/checks/", strings.NewReader(body))
	req.Header.Set("X-Api-Key", "the-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v3/checks/: %v", err)
	}
	resp.Body.Close()
}

// pause pauses the check at slug through the management API.
func pause(t *testing.T, server *healthcheckstest.Server, slug string) {
	t.Helper()
	check, _ := server.Check(slug)
	req, _ := http.NewRequest(http.MethodPost, server.URL()+"/api/v3/checks/"+check.UUID+"/pause", nil)
	req.Header.Set("X-Api-Key", "the-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST pause: %v", err)
	}
	resp.Body.Close()
}

// A ping moves a check's status the way hc.api.models.Check.ping does,
// and a change notifies the way hc.api.models.Flip.select_channels
// decides: a change to down notifies, and a change to up notifies
// unless the check was new or paused. A paused check with
// manual_resume set records each ping as ign and keeps its status.
func TestAPingMovesTheStatusAndNotifies(t *testing.T) {
	cases := []struct {
		name          string
		manualResume  bool
		paused        bool
		kind          string
		status        string
		notifications []string
	}{
		{"a success on a new check", false, false, "", "up", nil},
		{"a failure on a new check", false, false, "fail", "down", []string{"down"}},
		{"a start on a new check", false, false, "start", "new", nil},
		{"a success on a paused check", false, true, "", "up", nil},
		{"a failure on a paused check", false, true, "fail", "down", []string{"down"}},
		{"a success on a paused check held for a manual resume", true, true, "", "paused", nil},
		{"a failure on a paused check held for a manual resume", true, true, "fail", "paused", nil},
		{"a failure on a check that is not paused", true, false, "fail", "down", []string{"down"}},
		{"a log on a new check", false, false, "log", "new", nil},
		{"a log on a paused check", false, true, "log", "paused", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := healthcheckstest.NewServer("the-key")
			t.Cleanup(server.Close)
			upsert(t, server, "website", tc.manualResume)
			if tc.paused {
				pause(t, server, "website")
			}

			ping(t, server, "website", tc.kind)

			check, _ := server.Check("website")
			if check.Status != tc.status {
				t.Errorf("status = %q, want %q", check.Status, tc.status)
			}
			if got := server.Notifications(check.UUID); !slices.Equal(got, tc.notifications) {
				t.Errorf("notifications = %v, want %v", got, tc.notifications)
			}
		})
	}
}

func TestAnIgnoredPingIsRecordedAsIgn(t *testing.T) {
	server := healthcheckstest.NewServer("the-key")
	t.Cleanup(server.Close)
	upsert(t, server, "website", true)
	pause(t, server, "website")

	ping(t, server, "website", "fail")

	check, _ := server.Check("website")
	pings := server.Pings(check.UUID)
	if len(pings) != 1 || pings[0].Kind != "ign" {
		t.Errorf("pings = %+v, want one ign", pings)
	}
}

func TestAnUpdateSetsManualResume(t *testing.T) {
	server := healthcheckstest.NewServer("the-key")
	t.Cleanup(server.Close)
	upsert(t, server, "website", true)
	upsert(t, server, "website", false)

	check, _ := server.Check("website")
	if check.ManualResume {
		t.Error("ManualResume = true after an update that cleared it")
	}
}

func TestPausingWithTheWrongKeyIsUnauthorized(t *testing.T) {
	server := healthcheckstest.NewServer("the-key")
	t.Cleanup(server.Close)
	upsert(t, server, "website", false)
	check, _ := server.Check("website")

	req, _ := http.NewRequest(http.MethodPost, server.URL()+"/api/v3/checks/"+check.UUID+"/pause", nil)
	req.Header.Set("X-Api-Key", "the-wrong-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST pause: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}
