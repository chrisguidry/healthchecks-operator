package healthchecks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// Delete removes the check with the given UUID. A 404 means the check
// is already gone, which is the outcome Delete is for, so it is success
// rather than an error.
func (c *Client) Delete(ctx context.Context, uuid string) error {
	status, data, err := c.request(ctx, http.MethodDelete, "/api/v3/checks/"+uuid, nil)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return nil
	}
	if status != http.StatusOK {
		return &APIError{StatusCode: status, Body: string(data)}
	}
	return nil
}

// FindBySlug looks up the check at a slug, for example a slug a Check
// resource no longer carries because it changed. It returns nil, nil
// when no check has that slug.
func (c *Client) FindBySlug(ctx context.Context, slug string) (*Check, error) {
	status, data, err := c.request(ctx, http.MethodGet, "/api/v3/checks/?slug="+url.QueryEscape(slug), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &APIError{StatusCode: status, Body: string(data)}
	}

	var out struct {
		Checks []struct {
			UUID    string `json:"uuid"`
			PingURL string `json:"ping_url"`
			Slug    string `json:"slug"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("healthchecks: could not parse response: %w", err)
	}
	if len(out.Checks) == 0 {
		return nil, nil
	}
	found := out.Checks[0]
	return &Check{UUID: found.UUID, PingURL: found.PingURL, Slug: found.Slug}, nil
}
