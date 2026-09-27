package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// probeTimeout bounds one HTTP request, body included, or one TLS
// handshake. A target that stops answering fails the probe after this
// long, and it cannot hold a run past the next interval, which is at
// least a minute.
const probeTimeout = 10 * time.Second

// bodyLimit is how much of a response body the probe reads to look for
// bodyContains. A page is smaller than this, and a target that streams
// without end cannot hold a run or its memory.
const bodyLimit = 1 << 20

type httpProber struct {
	requests  []HTTPRequest
	namespace string
	secrets   secretReader
	// verified checks the server's certificate against the system
	// roots, and unverified skips the check for tlsVerify: false.
	verified   *http.Client
	unverified *http.Client
	timeout    time.Duration
}

// newHTTPProber builds the prober for a Check's http block. Header
// values from Secrets are read through secrets, in namespace, on each
// probe run.
func newHTTPProber(spec HTTPProbe, namespace string, secrets secretReader) (prober, error) {
	return newHTTPProberWithRoots(spec, namespace, secrets, nil)
}

// newHTTPProberWithRoots trusts roots in place of the system roots
// when roots is not nil, so a test trusts its own TLS server.
func newHTTPProberWithRoots(spec HTTPProbe, namespace string, secrets secretReader, roots *x509.CertPool) (*httpProber, error) {
	if len(spec.Requests) == 0 {
		return nil, errors.New("http has no requests")
	}
	for _, request := range spec.Requests {
		address, err := url.Parse(request.URL)
		if err != nil {
			return nil, err
		}
		if address.Scheme != "http" && address.Scheme != "https" {
			return nil, fmt.Errorf("%s: the scheme must be http or https", request.URL)
		}
	}
	return &httpProber{
		requests:   spec.Requests,
		namespace:  namespace,
		secrets:    secrets,
		verified:   probeClient(&tls.Config{RootCAs: roots}),
		unverified: probeClient(&tls.Config{InsecureSkipVerify: true}),
		timeout:    probeTimeout,
	}, nil
}

// probeClient returns the response to each request as it is, without
// following redirects, so a request can expect a 301 and its Location.
// Each probe opens new connections, so a run measures what a new
// visitor gets, and no idle connection stays open between runs.
func probeClient(config *tls.Config) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:             http.ProxyFromEnvironment,
			TLSClientConfig:   config,
			DisableKeepAlives: true,
			// A custom TLS config turns HTTP/2 off unless this is set.
			ForceAttemptHTTP2: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// probe sends each request in order and stops at the first that fails,
// because its reason is the one a person needs, and later requests
// often fail for the same cause.
func (p *httpProber) probe(ctx context.Context) probeResult {
	for _, request := range p.requests {
		if problem := p.send(ctx, request); problem != "" {
			return probeResult{reason: request.URL + ": " + problem}
		}
	}
	return probeResult{passed: true}
}

// send makes one request and returns what is wrong with the answer, or
// an empty string when the answer meets every expectation.
func (p *httpProber) send(ctx context.Context, request HTTPRequest) string {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, request.URL, nil)
	if err != nil {
		return err.Error()
	}
	for _, header := range request.Headers {
		value, err := p.headerValue(ctx, header)
		if err != nil {
			return fmt.Sprintf("header %s: %v", header.Name, err)
		}
		// Go writes the Host header from req.Host and ignores a Host
		// entry in req.Header.
		if http.CanonicalHeaderKey(header.Name) == "Host" {
			req.Host = value
		} else {
			req.Header.Add(header.Name, value)
		}
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "healthchecks-operator")
	}
	client := p.verified
	if request.TLSVerify != nil && !*request.TLSVerify {
		client = p.unverified
	}
	resp, err := client.Do(req)
	if err != nil {
		return p.describeError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	return p.compare(resp, request.Expect)
}

// headerValue reads a Secret on every call, so a rotated token takes
// effect on the next probe without a watch on Secrets.
func (p *httpProber) headerValue(ctx context.Context, header RequestHeader) (string, error) {
	if header.ValueFrom == nil {
		return header.Value, nil
	}
	ref := header.ValueFrom.SecretKeyRef
	return p.secrets.secretValue(ctx, p.namespace, ref.Name, ref.Key)
}

// describeError drops the method and URL that net/http puts in front
// of its errors, because the reason already starts with the URL.
func (p *httpProber) describeError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("no answer within %s", p.timeout)
	}
	if urlError, ok := errors.AsType[*url.Error](err); ok {
		return urlError.Err.Error()
	}
	return err.Error()
}

// compare checks the status, then each expected header in name order,
// then the body, and returns the first problem it finds.
func (p *httpProber) compare(resp *http.Response, expect ResponseExpectation) string {
	if len(expect.Status) > 0 && !slices.Contains(expect.Status, int32(resp.StatusCode)) {
		return fmt.Sprintf("status %d, want %s", resp.StatusCode, eitherStatus(expect.Status))
	}
	for _, name := range slices.Sorted(maps.Keys(expect.Headers)) {
		want := expect.Headers[name]
		got := resp.Header.Values(name)
		if len(got) == 0 {
			return fmt.Sprintf("header %s is missing, want %q", name, want)
		}
		if got[0] != want {
			return fmt.Sprintf("header %s is %q, want %q", name, got[0], want)
		}
	}
	if expect.BodyContains == "" {
		return ""
	}
	// One byte past the limit tells a body of exactly the limit from a
	// longer one.
	body, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit+1))
	if err != nil {
		return p.describeError(fmt.Errorf("reading the body: %w", err))
	}
	if bytes.Contains(body[:min(len(body), bodyLimit)], []byte(expect.BodyContains)) {
		return ""
	}
	if len(body) > bodyLimit {
		return fmt.Sprintf("the first 1 MiB of the body does not contain %q", expect.BodyContains)
	}
	return fmt.Sprintf("body does not contain %q", expect.BodyContains)
}

// eitherStatus lists the statuses a request accepts the way a person
// says it: "200", "200 or 401", or "200, 204, or 401".
func eitherStatus(statuses []int32) string {
	words := make([]string, len(statuses))
	for i, status := range statuses {
		words[i] = fmt.Sprint(status)
	}
	switch len(words) {
	case 1:
		return words[0]
	case 2:
		return words[0] + " or " + words[1]
	}
	return strings.Join(words[:len(words)-1], ", ") + ", or " + words[len(words)-1]
}
