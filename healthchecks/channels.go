package healthchecks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Channel is one notification integration in a project, for example an
// email address or a Pushover account. The v3 API has no endpoint to
// create, update, or delete one; a project's channels are made on the
// Healthchecks web UI, and this client only reads them, to resolve the
// names a ClusterProject or Check declares to the IDs Upsert sends.
type Channel struct {
	ID   string
	Name string
	Kind string
}

// Channels lists every notification channel in the project.
func (c *Client) Channels(ctx context.Context) ([]Channel, error) {
	status, data, err := c.request(ctx, http.MethodGet, "/api/v3/channels/", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &APIError{StatusCode: status, Body: string(data)}
	}

	var out struct {
		Channels []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("healthchecks: could not parse response: %w", err)
	}

	channels := make([]Channel, len(out.Channels))
	for i, ch := range out.Channels {
		channels[i] = Channel{ID: ch.ID, Name: ch.Name, Kind: ch.Kind}
	}
	return channels, nil
}
