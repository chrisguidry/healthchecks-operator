package healthchecks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Period is how often a check expects a ping: either a fixed timeout
// after the last ping, or a cron or OnCalendar schedule evaluated in a
// timezone. A Check has exactly one, never both, so Period is built by
// FixedTimeout or CronSchedule rather than as a struct literal, which
// would let a caller set both, or neither, kinds at once.
type Period struct {
	timeout  time.Duration
	schedule string
	tz       string
}

// FixedTimeout builds a Period that expects a ping every d.
func FixedTimeout(d time.Duration) Period {
	return Period{timeout: d}
}

// CronSchedule builds a Period from a cron or OnCalendar expression,
// evaluated in tz.
func CronSchedule(expression, tz string) Period {
	return Period{schedule: expression, tz: tz}
}

func (p Period) isZero() bool {
	return p.timeout == 0 && p.schedule == ""
}

// UpsertRequest is what one check should become. Upsert sends every
// field on every call, including an empty Tags or Channels: Healthchecks
// reads a field's absence as "leave this alone", and this project
// declares a check's whole state from a Check resource on every
// reconcile, so a partial request could leave a removed tag or channel
// in place.
//
// Sending Period is the one exception. Upsert sends only the fields for
// the Period it carries: an absent schedule tells Healthchecks the check
// now has a fixed timeout, and an absent timeout tells it the check now
// runs on a schedule. Sending both, or the one the check is leaving,
// would not switch its kind.
type UpsertRequest struct {
	Name        string
	Slug        string
	Description string
	Tags        []string
	Channels    []string // channel IDs, not names
	Grace       time.Duration
	Period      Period
}

// Check identifies one check in Healthchecks.
type Check struct {
	UUID    string
	PingURL string
	Slug    string
}

// UpsertResult is a Check plus whether Upsert created it.
type UpsertResult struct {
	Check
	Created bool
}

// wireUpsert is the JSON body of a POST to /api/v3/checks/. Grace, Tags,
// and Channels carry no omitempty: their zero value is a meaningful
// request to clear the field, not an absent one. Timeout, Schedule, and
// TZ do carry it, because whichever half of Period is unset must be
// absent from the request rather than sent as zero.
type wireUpsert struct {
	Name     string   `json:"name"`
	Slug     string   `json:"slug"`
	Desc     string   `json:"desc"`
	Tags     string   `json:"tags"`
	Channels string   `json:"channels"`
	Grace    int      `json:"grace"`
	Timeout  int      `json:"timeout,omitempty"`
	Schedule string   `json:"schedule,omitempty"`
	TZ       string   `json:"tz,omitempty"`
	Unique   []string `json:"unique"`
}

// Upsert creates the check at req.Slug, or updates it if a check with
// that slug already exists. Healthchecks decides which by the "unique"
// parameter: this client always sends ["slug"], because the slug, not a
// stored UUID, is what identifies a Check resource's check across a
// reinstall or a rebuilt cluster.
func (c *Client) Upsert(ctx context.Context, req UpsertRequest) (*UpsertResult, error) {
	if req.Slug == "" {
		return nil, fmt.Errorf("healthchecks: Upsert requires a slug")
	}
	if req.Period.isZero() {
		return nil, fmt.Errorf("healthchecks: Upsert requires a Period")
	}

	wire := wireUpsert{
		Name:     req.Name,
		Slug:     req.Slug,
		Desc:     req.Description,
		Tags:     strings.Join(req.Tags, " "),
		Channels: strings.Join(req.Channels, ","),
		Grace:    int(req.Grace.Seconds()),
		Timeout:  int(req.Period.timeout.Seconds()),
		Schedule: req.Period.schedule,
		TZ:       req.Period.tz,
		Unique:   []string{"slug"},
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}

	status, data, err := c.request(ctx, http.MethodPost, "/api/v3/checks/", body)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return nil, &APIError{StatusCode: status, Body: string(data)}
	}

	var out struct {
		UUID    string `json:"uuid"`
		PingURL string `json:"ping_url"`
		Slug    string `json:"slug"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("healthchecks: could not parse response: %w", err)
	}
	return &UpsertResult{
		Check:   Check{UUID: out.UUID, PingURL: out.PingURL, Slug: out.Slug},
		Created: status == http.StatusCreated,
	}, nil
}
