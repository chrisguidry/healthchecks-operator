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
	"net/http/cookiejar"
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
	verified   *http.Transport
	unverified *http.Transport
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
		verified:   probeTransport(&tls.Config{RootCAs: roots}),
		unverified: probeTransport(&tls.Config{InsecureSkipVerify: true}),
		timeout:    probeTimeout,
	}, nil
}

// probeTransport opens new connections for each probe, so a run
// measures what a new visitor gets, and no idle connection stays open
// between runs.
func probeTransport(config *tls.Config) *http.Transport {
	return &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		TLSClientConfig:   config,
		DisableKeepAlives: true,
		// A custom TLS config turns HTTP/2 off unless this is set.
		ForceAttemptHTTP2: true,
	}
}

// probeClient returns the response to a request as it is, so a request
// can expect a 301 and its Location, unless the request follows
// redirects.
//
// A request that follows redirects keeps its Host header only on a
// relative redirect: an absolute Location names its host, as Go's client
// does. A redirect to another host carries none of the request's own
// headers, because they may hold a Secret meant for the first host, and
// a redirect from https to http is refused, because it would send them
// in the clear. A cookie jar lasts for the one request, so a sign-in
// page that sets a cookie and redirects back sees it.
//
// hop holds the URL of the last redirect the client followed, so a
// failure after a redirect can name where it happened.
func probeClient(transport *http.Transport, request HTTPRequest, hop **url.URL) *http.Client {
	client := &http.Client{Transport: transport}
	if !request.FollowRedirects {
		client.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
		return client
	}
	client.Jar, _ = cookiejar.New(nil)
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		*hop = next.URL
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		first := via[0].URL
		if first.Scheme == "https" && next.URL.Scheme == "http" {
			return errors.New("refused to follow a redirect from https to http")
		}
		if next.URL.Host != first.Host {
			for _, header := range request.Headers {
				next.Header.Del(header.Name)
			}
			// Go sets Referer to the previous hop, query included.
			next.Header.Del("Referer")
		}
		return nil
	}
	return client
}

// maxRedirects is the number of redirects a request follows, the same
// limit as Go's client.
const maxRedirects = 10

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
	transport := p.verified
	if request.TLSVerify != nil && !*request.TLSVerify {
		transport = p.unverified
	}
	var hop *url.URL
	resp, err := probeClient(transport, request, &hop).Do(req)
	if err != nil {
		return p.describeError(err, hop)
	}
	defer func() { _ = resp.Body.Close() }()
	if problem := p.compare(resp, request.Expect); problem != "" {
		return problem + landedAt(hop)
	}
	return ""
}

// landedAt names where a followed request landed, for a reason that would
// otherwise blame the URL it started from. It leaves out the userinfo,
// the query, and the fragment, because a sign-in redirect carries
// passwords and tokens there.
func landedAt(hop *url.URL) string {
	if hop == nil {
		return ""
	}
	landed := *hop
	landed.User, landed.RawQuery, landed.Fragment = nil, "", ""
	return ", at " + landed.String()
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
func (p *httpProber) describeError(err error, hop *url.URL) string {
	problem := err.Error()
	if errors.Is(err, context.DeadlineExceeded) {
		problem = fmt.Sprintf("no answer within %s", p.timeout)
	} else if urlError, ok := errors.AsType[*url.Error](err); ok {
		problem = urlError.Err.Error()
	}
	// After a redirect, the error belongs to the hop, not to the URL the
	// reason starts with.
	return problem + landedAt(hop)
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
		return p.describeError(fmt.Errorf("reading the body: %w", err), nil)
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
