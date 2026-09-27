package healthchecks

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Client reaches one project on one Healthchecks instance. Every
// Healthchecks v3 API key belongs to exactly one project, so a Client
// needs no project argument on its methods.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewClient builds a Client for one project. baseURL is the instance's
// root, for example "https://healthchecks.example.com" or
// "https://healthchecks.io"; apiKey must be a read-write key, since a
// read-only key cannot create a check or return its ping_url. A nil
// httpClient uses NewHTTPClient with no timeout.
func NewClient(baseURL, apiKey string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = defaultHTTPClient
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    httpClient,
	}
}

// APIError is a Healthchecks response outside the 2xx range. StatusCode
// and Body carry the server's own answer word for word, so a caller can
// put Healthchecks' own explanation into a log line or a status
// condition without paraphrasing it.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("healthchecks: %s: %s", http.StatusText(e.StatusCode), e.Body)
}

// request performs one call against the management API and returns the
// status code and body. The caller decides which status codes are
// success, since a 404 on Delete is not an error but a 404 elsewhere is.
func (c *Client) request(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, data, nil
}
