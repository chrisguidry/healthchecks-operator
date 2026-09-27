// Package healthcheckstest is a stateful fake of one Healthchecks
// project, for another package's tests to run a healthchecks.Client
// against instead of the real API. It holds checks and channels in
// memory and implements the endpoints healthchecks.Client calls, with
// the real server's own behavior: an upsert keyed on slug answers 200 on
// update and 201 on create, a missing or wrong X-Api-Key answers 401,
// and each check's ping_url records the pings sent to it. See the
// healthchecks package doc comment for the sources this behavior comes
// from.
package healthcheckstest

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"time"
)

// Server is a fake Healthchecks instance holding one project.
type Server struct {
	server *httptest.Server
	apiKey string

	mu       sync.Mutex
	checks   map[string]*storedCheck // by slug
	byUUID   map[string]*storedCheck // by uuid
	channels []storedChannel
	pings    map[string][]Ping // by check uuid, in the order they arrived
}

type storedCheck struct {
	uuid     string
	slug     string
	name     string
	desc     string
	tags     string
	channels string
	grace    int
	timeout  int
	schedule string
	tz       string
}

type storedChannel struct {
	id   string
	name string
	kind string
}

// Ping is one recorded call to a check's ping URL.
type Ping struct {
	Kind string // "success", "start", or "fail"
	Body string
	At   time.Time
}

// NewServer starts a fake Healthchecks instance that accepts apiKey as
// its project's read-write key.
func NewServer(apiKey string) *Server {
	s := &Server{
		apiKey: apiKey,
		checks: map[string]*storedCheck{},
		byUUID: map[string]*storedCheck{},
		pings:  map[string][]Ping{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v3/checks/", s.handleUpsert)
	mux.HandleFunc("GET /api/v3/checks/", s.handleFindBySlug)
	mux.HandleFunc("DELETE /api/v3/checks/{uuid}", s.handleDelete)
	mux.HandleFunc("GET /api/v3/channels/", s.handleChannels)
	mux.HandleFunc("/ping/{uuid}", s.handlePing(""))
	mux.HandleFunc("/ping/{uuid}/start", s.handlePing("start"))
	mux.HandleFunc("/ping/{uuid}/fail", s.handlePing("fail"))
	s.server = httptest.NewServer(mux)

	return s
}

// URL is the base URL a healthchecks.Client should use as this project's
// instance.
func (s *Server) URL() string { return s.server.URL }

// Close shuts down the underlying httptest.Server.
func (s *Server) Close() { s.server.Close() }

// SeedChannel adds a channel to the project, as if it had been created
// on the Healthchecks web UI, and returns its ID.
func (s *Server) SeedChannel(name, kind string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := newUUID()
	s.channels = append(s.channels, storedChannel{id: id, name: name, kind: kind})
	return id
}

// Check reads back the check at a slug, for a test to assert on what a
// Client sent. The second result is false when no check has that slug.
func (s *Server) Check(slug string) (StoredCheck, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.checks[slug]
	if !ok {
		return StoredCheck{}, false
	}
	return c.export(s.server.URL), true
}

// Pings reads back every ping recorded for a check's UUID, oldest first.
func (s *Server) Pings(uuid string) []Ping {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Ping(nil), s.pings[uuid]...)
}

// StoredCheck is a check as the fake server holds it, for a test to
// assert on.
type StoredCheck struct {
	UUID        string
	Slug        string
	Name        string
	Description string
	Tags        []string
	Channels    []string // channel IDs
	Grace       time.Duration
	Timeout     time.Duration // zero when the check runs on a Schedule instead
	Schedule    string
	Timezone    string
	PingURL     string
}

func (c *storedCheck) export(baseURL string) StoredCheck {
	return StoredCheck{
		UUID:        c.uuid,
		Slug:        c.slug,
		Name:        c.name,
		Description: c.desc,
		Tags:        splitNonEmpty(c.tags, " "),
		Channels:    splitNonEmpty(c.channels, ","),
		Grace:       time.Duration(c.grace) * time.Second,
		Timeout:     time.Duration(c.timeout) * time.Second,
		Schedule:    c.schedule,
		Timezone:    c.tz,
		PingURL:     baseURL + "/ping/" + c.uuid,
	}
}

func splitNonEmpty(s, sep string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, sep)
}

func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Api-Key") != s.apiKey {
		writeError(w, http.StatusUnauthorized, "wrong api key")
		return false
	}
	return true
}

// handleUpsert implements POST /api/v3/checks/: create, or update the
// check found by the "unique" fields, matching hc.api.views._lookup and
// hc.api.views._update. This fake supports the one unique key this
// project's client sends, "slug".
func (s *Server) handleUpsert(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}

	var body struct {
		Name     string   `json:"name"`
		Slug     string   `json:"slug"`
		Desc     string   `json:"desc"`
		Tags     string   `json:"tags"`
		Channels string   `json:"channels"`
		Grace    int      `json:"grace"`
		Timeout  int      `json:"timeout"`
		Schedule string   `json:"schedule"`
		TZ       string   `json:"tz"`
		Unique   []string `json:"unique"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "could not parse request body")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var check *storedCheck
	if slices.Contains(body.Unique, "slug") {
		check = s.checks[body.Slug]
	}

	created := check == nil
	if created {
		check = &storedCheck{uuid: newUUID()}
	}

	check.name = body.Name
	check.slug = body.Slug
	check.desc = body.Desc
	check.tags = body.Tags
	check.channels = body.Channels
	check.grace = body.Grace
	// The kind switches on which of schedule and timeout the request
	// carries, the same as the real server: an absent schedule means
	// "use timeout", and an absent timeout means "keep the schedule".
	if body.Schedule != "" {
		check.schedule = body.Schedule
		check.tz = body.TZ
		check.timeout = 0
	} else if body.Timeout != 0 {
		check.timeout = body.Timeout
		check.schedule = ""
		check.tz = ""
	}

	s.checks[check.slug] = check
	s.byUUID[check.uuid] = check

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, s.dict(check))
}

// handleFindBySlug implements GET /api/v3/checks/?slug=...
func (s *Server) handleFindBySlug(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	checks := []map[string]any{}
	if check, ok := s.checks[r.URL.Query().Get("slug")]; ok {
		checks = append(checks, s.dict(check))
	}
	writeJSON(w, http.StatusOK, map[string]any{"checks": checks})
}

// handleDelete implements DELETE /api/v3/checks/<uuid>. A UUID this
// project has never seen is 404, which healthchecks.Client.Delete treats
// as success: the check is already gone.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	uuid := r.PathValue("uuid")
	check, ok := s.byUUID[uuid]
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	delete(s.byUUID, uuid)
	delete(s.checks, check.slug)
	delete(s.pings, uuid)
	writeJSON(w, http.StatusOK, s.dict(check))
}

// handleChannels implements GET /api/v3/channels/.
func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	channels := make([]map[string]any, len(s.channels))
	for i, ch := range s.channels {
		channels[i] = map[string]any{"id": ch.id, "name": ch.name, "kind": ch.kind}
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": channels})
}

// handlePing implements a check's ping_url and its "/start" and "/fail"
// suffixes. Pinging carries no API key: the UUID in the URL is the
// credential, the same as the real hc-ping.com.
func (s *Server) handlePing(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uuid := r.PathValue("uuid")

		s.mu.Lock()
		_, known := s.byUUID[uuid]
		if known {
			body := readAndCapBody(r)
			s.pings[uuid] = append(s.pings[uuid], Ping{Kind: pingKindName(kind), Body: body, At: time.Now()})
		}
		s.mu.Unlock()

		if !known {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		fmt.Fprint(w, "OK")
	}
}

func pingKindName(suffix string) string {
	if suffix == "" {
		return "success"
	}
	return suffix
}

func (s *Server) dict(c *storedCheck) map[string]any {
	result := map[string]any{
		"name":     c.name,
		"slug":     c.slug,
		"desc":     c.desc,
		"tags":     c.tags,
		"grace":    c.grace,
		"uuid":     c.uuid,
		"ping_url": s.server.URL + "/ping/" + c.uuid,
		"channels": c.channels,
	}
	if c.schedule != "" {
		result["schedule"] = c.schedule
		result["tz"] = c.tz
	} else {
		result["timeout"] = c.timeout
	}
	return result
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// readAndCapBody reads a ping's body, capped at 100 kB, matching what
// the real Healthchecks keeps. The client already caps what it sends,
// so this only guards the fake against a test that sends more.
func readAndCapBody(r *http.Request) string {
	const maxPingBodyBytes = 100_000
	data, _ := io.ReadAll(io.LimitReader(r.Body, maxPingBodyBytes))
	return string(data)
}

// newUUID generates a fake but well-formed v4 UUID, so a check's ID and
// ping_url look like a real check's.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
