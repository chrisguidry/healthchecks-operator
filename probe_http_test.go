package main

import (
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// probeTarget answers the paths the http probe tests request, and
// records each path it answers.
type probeTarget struct {
	mutex sync.Mutex
	paths []string
}

func (target *probeTarget) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target.mutex.Lock()
	target.paths = append(target.paths, r.URL.Path)
	target.mutex.Unlock()
	switch r.URL.Path {
	case "/":
		fmt.Fprint(w, "Welcome to example.com")
	case "/moved":
		http.Redirect(w, r, "https://example.com/", http.StatusMovedPermanently)
	case "/broken":
		w.WriteHeader(http.StatusBadGateway)
	case "/echo":
		fmt.Fprintf(w, "host=%s authorization=%s", r.Host, r.Header.Get("Authorization"))
	case "/large":
		fmt.Fprint(w, strings.Repeat("a", bodyLimit), "Welcome")
	case "/slow":
		<-r.Context().Done()
	}
}

func (target *probeTarget) requested() string {
	target.mutex.Lock()
	defer target.mutex.Unlock()
	return strings.Join(target.paths, ",")
}

type httpProbeHarness struct {
	target *probeTarget
	api    *fakeKube
	// urls replaces {http} and {https} in a case with the servers'
	// addresses.
	urls  *strings.Replacer
	roots *x509.CertPool
}

// startProbeTargets serves one probeTarget over HTTP and over HTTPS,
// and starts a fake API server with a Secret for header values.
func startProbeTargets(t *testing.T) *httpProbeHarness {
	t.Helper()
	target := &probeTarget{}
	plain := httptest.NewServer(target)
	t.Cleanup(plain.Close)
	secure := httptest.NewUnstartedServer(target)
	// A rejected handshake is the outcome some tests want, and the
	// server's log line for it is noise.
	secure.Config.ErrorLog = log.New(io.Discard, "", 0)
	secure.StartTLS()
	t.Cleanup(secure.Close)
	roots := x509.NewCertPool()
	roots.AddCert(secure.Certificate())

	api := startFakeKube(t)
	api.create(secretsResource, secretWith("example", "probe-token", map[string][]byte{"header": []byte("Bearer abc")}))
	return &httpProbeHarness{
		target: target,
		api:    api,
		urls:   strings.NewReplacer("{http}", plain.URL, "{https}", secure.URL),
		roots:  roots,
	}
}

// probeSpec fills in the servers' addresses in each request's URL.
func (h *httpProbeHarness) probeSpec(requests []HTTPRequest) HTTPProbe {
	spec := HTTPProbe{Interval: "5m"}
	for _, request := range requests {
		request.URL = h.urls.Replace(request.URL)
		spec.Requests = append(spec.Requests, request)
	}
	return spec
}

func secretHeader(name, secret, key string) RequestHeader {
	return RequestHeader{Name: name, ValueFrom: &HeaderValueFrom{SecretKeyRef{Name: secret, Key: key}}}
}

func TestHTTPProbeResults(t *testing.T) {
	cases := []struct {
		name     string
		requests []HTTPRequest
		reason   string
	}{
		{"any status passes without expectations",
			[]HTTPRequest{{URL: "{http}/broken"}}, ""},
		{"status in the list",
			[]HTTPRequest{{URL: "{http}/", Expect: ResponseExpectation{Status: []int32{200, 401}}}}, ""},
		{"status outside the list",
			[]HTTPRequest{{URL: "{http}/broken", Expect: ResponseExpectation{Status: []int32{200, 401}}}},
			"{http}/broken: status 502, want 200 or 401"},
		{"status outside a list of three",
			[]HTTPRequest{{URL: "{http}/broken", Expect: ResponseExpectation{Status: []int32{200, 204, 401}}}},
			"{http}/broken: status 502, want 200, 204, or 401"},
		{"redirect is not followed",
			[]HTTPRequest{{URL: "{http}/moved", Expect: ResponseExpectation{
				Status:  []int32{301},
				Headers: map[string]string{"Location": "https://example.com/"}}}}, ""},
		{"header differs",
			[]HTTPRequest{{URL: "{http}/moved", Expect: ResponseExpectation{
				Headers: map[string]string{"Location": "https://example.net/"}}}},
			`{http}/moved: header Location is "https://example.com/", want "https://example.net/"`},
		{"header is missing",
			[]HTTPRequest{{URL: "{http}/", Expect: ResponseExpectation{
				Headers: map[string]string{"Location": "https://example.com/"}}}},
			`{http}/: header Location is missing, want "https://example.com/"`},
		{"body contains the text",
			[]HTTPRequest{{URL: "{http}/", Expect: ResponseExpectation{BodyContains: "Welcome"}}}, ""},
		{"body lacks the text",
			[]HTTPRequest{{URL: "{http}/", Expect: ResponseExpectation{BodyContains: "Goodbye"}}},
			`{http}/: body does not contain "Goodbye"`},
		{"text past the first MiB",
			[]HTTPRequest{{URL: "{http}/large", Expect: ResponseExpectation{BodyContains: "Welcome"}}},
			`{http}/large: the first 1 MiB of the body does not contain "Welcome"`},
		{"Host and a Secret header are sent",
			[]HTTPRequest{{URL: "{http}/echo",
				Headers: []RequestHeader{{Name: "host", Value: "example.com"}, secretHeader("Authorization", "probe-token", "header")},
				Expect:  ResponseExpectation{BodyContains: "host=example.com authorization=Bearer abc"}}}, ""},
		{"Secret is missing",
			[]HTTPRequest{{URL: "{http}/echo", Headers: []RequestHeader{secretHeader("Authorization", "missing", "header")}}},
			`{http}/echo: header Authorization: reading key "header" of Secret example/missing: not found`},
		{"certificate is trusted",
			[]HTTPRequest{{URL: "{https}/", Expect: ResponseExpectation{Status: []int32{200}}}}, ""},
		{"the second request fails",
			[]HTTPRequest{
				{URL: "{https}/", Expect: ResponseExpectation{Status: []int32{200}}},
				{URL: "{https}/broken", Expect: ResponseExpectation{Status: []int32{200}}}},
			"{https}/broken: status 502, want 200"},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			h := startProbeTargets(t)
			p, err := newHTTPProberWithRoots(h.probeSpec(one.requests), "example", h.api.client, h.roots)
			mustSucceed(t, err)

			result := p.probe(t.Context())

			mustMatch(t, result, probeResult{passed: one.reason == "", reason: h.urls.Replace(one.reason)})
		})
	}
}

func TestHTTPProbeStopsAtTheFirstFailure(t *testing.T) {
	h := startProbeTargets(t)
	p, err := newHTTPProberWithRoots(h.probeSpec([]HTTPRequest{
		{URL: "{http}/broken", Expect: ResponseExpectation{Status: []int32{200}}},
		{URL: "{http}/"},
	}), "example", h.api.client, h.roots)
	mustSucceed(t, err)

	p.probe(t.Context())

	mustMatch(t, h.target.requested(), "/broken")
}

func TestHTTPProbeGivesUpOnATargetThatDoesNotAnswer(t *testing.T) {
	h := startProbeTargets(t)
	p, err := newHTTPProberWithRoots(h.probeSpec([]HTTPRequest{{URL: "{http}/slow"}}), "example", h.api.client, h.roots)
	mustSucceed(t, err)
	p.timeout = 50 * time.Millisecond

	result := p.probe(t.Context())

	mustMatch(t, result, probeResult{reason: h.urls.Replace("{http}/slow: no answer within 50ms")})
}

// The probe trusts no root here, so only a request that turns
// verification off reaches the HTTPS server.
func TestHTTPProbeVerifiesTheCertificateUnlessARequestTurnsItOff(t *testing.T) {
	off, on := false, true
	cases := []struct {
		name   string
		verify *bool
		reason string
	}{
		{"default", nil, "{https}/: tls: failed to verify certificate: x509: certificate signed by unknown authority"},
		{"on", &on, "{https}/: tls: failed to verify certificate: x509: certificate signed by unknown authority"},
		{"off", &off, ""},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			h := startProbeTargets(t)
			spec := h.probeSpec([]HTTPRequest{{URL: "{https}/", TLSVerify: one.verify}})
			p, err := newHTTPProberWithRoots(spec, "example", h.api.client, x509.NewCertPool())
			mustSucceed(t, err)

			result := p.probe(t.Context())

			mustMatch(t, result, probeResult{passed: one.reason == "", reason: h.urls.Replace(one.reason)})
		})
	}
}

func TestHTTPProberRejectsASpecItCannotSend(t *testing.T) {
	cases := []struct {
		name     string
		requests []HTTPRequest
		want     string
	}{
		{"no requests", nil, "http has no requests"},
		{"bad URL", []HTTPRequest{{URL: "http://example.com/%zz"}}, `parse "http://example.com/%zz": invalid URL escape "%zz"`},
		{"other scheme", []HTTPRequest{{URL: "ftp://example.com/"}}, "ftp://example.com/: the scheme must be http or https"},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			_, err := newHTTPProber(HTTPProbe{Interval: "5m", Requests: one.requests}, "example", nil)

			mustMatch(t, err.Error(), one.want)
		})
	}
}
