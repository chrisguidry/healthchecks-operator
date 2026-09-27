package healthchecks

import (
	"context"
	"io"
	"net/http"
	"strings"
)

// maxPingBodyBytes matches what Healthchecks keeps: "Healthchecks.io
// stores the first 100 kB of the request body" (Pinging API docs). A
// probe's failure reason rarely nears this, but sending less than the
// server would keep anyway avoids relying on the server to truncate a
// body this client generated.
const maxPingBodyBytes = 100_000

// PingKind is which signal a ping sends.
type PingKind int

const (
	// PingSuccess reports that a check's job finished and everything it
	// checked passed.
	PingSuccess PingKind = iota
	// PingStart reports that a check's job began, so Healthchecks can
	// time the run and flag it if the run outlasts the check's grace
	// period.
	PingStart
	// PingFail reports that a check's job finished but something it
	// checked did not pass.
	PingFail
)

func (k PingKind) suffix() string {
	switch k {
	case PingStart:
		return "/start"
	case PingFail:
		return "/fail"
	default:
		return ""
	}
}

// Ping sends one signal to a check's ping URL: a Check's ping_url for
// PingSuccess, or that URL plus "/start" or "/fail". Pinging needs no
// API key; the URL itself, minted when the check was created, is the
// credential. body is optional and is sent as text/plain, cut to 100 kB.
//
// A nil client uses NewHTTPClient with no timeout.
func Ping(ctx context.Context, client *http.Client, pingURL string, kind PingKind, body string) error {
	if client == nil {
		client = defaultHTTPClient
	}
	if len(body) > maxPingBodyBytes {
		body = body[:maxPingBodyBytes]
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pingURL+kind.suffix(), strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &APIError{StatusCode: resp.StatusCode, Body: string(data)}
	}
	return nil
}
