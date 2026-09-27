package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// tlsProbeNow is the tls probe tests' clock.
var tlsProbeNow = time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)

// certificateUntil makes a self-signed certificate for 127.0.0.1 that
// is valid from a day before tlsProbeNow until notAfter.
func certificateUntil(t *testing.T, notAfter time.Time) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	mustSucceed(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    tlsProbeNow.Add(-24 * time.Hour),
		NotAfter:     notAfter,
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	mustSucceed(t, err)
	leaf, err := x509.ParseCertificate(der)
	mustSucceed(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// serveCertificate starts a TLS server with certificate, and returns
// its host:port and a pool that trusts the certificate.
func serveCertificate(t *testing.T, certificate tls.Certificate) (string, *x509.CertPool) {
	t.Helper()
	server := httptest.NewUnstartedServer(http.NotFoundHandler())
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
	// A rejected handshake is the outcome some tests want, and the
	// server's log line for it is noise.
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(certificate.Leaf)
	return server.Listener.Addr().String(), roots
}

func TestTLSProbeResults(t *testing.T) {
	cases := []struct {
		name     string
		notAfter time.Time
		reason   string
	}{
		{"enough left", tlsProbeNow.Add(30 * 24 * time.Hour), ""},
		{"exactly the minimum left", tlsProbeNow.Add(14 * 24 * time.Hour), ""},
		{"too little left", time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC),
			"{host}: certificate expires 2026-10-03T00:00:00Z, 6d left, want at least 14d"},
		{"less than a day left", tlsProbeNow.Add(5*time.Hour + 30*time.Minute + 20*time.Second),
			"{host}: certificate expires 2026-09-27T05:30:20Z, 5h30m left, want at least 14d"},
		{"expired", tlsProbeNow.Add(-time.Hour),
			"{host}: tls: failed to verify certificate: x509: certificate has expired or is not yet valid: " +
				"current time 2026-09-27T00:00:00Z is after 2026-09-26T23:00:00Z"},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			host, roots := serveCertificate(t, certificateUntil(t, one.notAfter))
			p, err := newTLSProberWith(TLSProbe{Interval: "24h", Host: host, MinRemaining: "336h"}, roots,
				func() time.Time { return tlsProbeNow })
			mustSucceed(t, err)

			result := p.probe(t.Context())

			mustMatch(t, result, probeResult{passed: one.reason == "", reason: strings.ReplaceAll(one.reason, "{host}", host)})
		})
	}
}

func TestTLSProbeFailsForAnUntrustedCertificate(t *testing.T) {
	host, _ := serveCertificate(t, certificateUntil(t, tlsProbeNow.Add(30*24*time.Hour)))
	p, err := newTLSProberWith(TLSProbe{Interval: "24h", Host: host, MinRemaining: "336h"}, x509.NewCertPool(),
		func() time.Time { return tlsProbeNow })
	mustSucceed(t, err)

	result := p.probe(t.Context())

	mustMatch(t, result, probeResult{reason: host + ": tls: failed to verify certificate: x509: certificate signed by unknown authority"})
}

// closedAddress is an address on this machine that refuses
// connections: a listener's port, after the listener closed.
func closedAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	mustSucceed(t, err)
	address := listener.Addr().String()
	mustSucceed(t, listener.Close())
	return address
}

func TestTLSProbeFailsWhenTheHostRefusesTheConnection(t *testing.T) {
	host := closedAddress(t)
	p, err := newTLSProber(TLSProbe{Interval: "24h", Host: host, MinRemaining: "336h"})
	mustSucceed(t, err)

	result := p.probe(t.Context())

	mustMatch(t, result, probeResult{reason: host + ": dial tcp " + host + ": connect: connection refused"})
}

// silentAddress accepts connections and never answers, so a handshake
// with it waits until the probe's timeout.
func silentAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	mustSucceed(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String()
}

func TestTLSProbeGivesUpOnAHostThatDoesNotAnswer(t *testing.T) {
	host := silentAddress(t)
	p, err := newTLSProberWith(TLSProbe{Interval: "24h", Host: host, MinRemaining: "336h"}, nil, time.Now)
	mustSucceed(t, err)
	p.timeout = 50 * time.Millisecond

	result := p.probe(t.Context())

	mustMatch(t, result, probeResult{reason: host + ": no answer within 50ms"})
}

func TestTLSProberRejectsASpecItCannotProbe(t *testing.T) {
	cases := []struct {
		host         string
		minRemaining string
		want         string
	}{
		{"example.com:https:443", "336h", `tls.host "example.com:https:443" is not host or host:port`},
		{"example.com:443", "two weeks", `tls.minRemaining: time: invalid duration "two weeks"`},
		{"example.com:443", "0s", "tls.minRemaining 0s is not positive"},
	}
	for _, one := range cases {
		t.Run(one.want, func(t *testing.T) {
			_, err := newTLSProber(TLSProbe{Interval: "24h", Host: one.host, MinRemaining: one.minRemaining})

			mustMatch(t, err.Error(), one.want)
		})
	}
}

func TestTLSProberDefaultsToPort443(t *testing.T) {
	cases := []struct {
		host       string
		want       string
		serverName string
	}{
		{"example.com", "example.com:443", "example.com"},
		{"example.com:8443", "example.com:8443", "example.com"},
		{"2001:db8::1", "[2001:db8::1]:443", "2001:db8::1"},
	}
	for _, one := range cases {
		t.Run(one.host, func(t *testing.T) {
			p, err := newTLSProberWith(TLSProbe{Interval: "24h", Host: one.host, MinRemaining: "336h"}, nil, time.Now)

			mustSucceed(t, err)
			mustMatch(t, p.host, one.want)
			mustMatch(t, p.serverName, one.serverName)
		})
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		duration time.Duration
		want     string
	}{
		{336 * time.Hour, "14d"},
		{36 * time.Hour, "1d12h"},
		{5*time.Hour + 30*time.Minute + 20*time.Second, "5h30m"},
		{30 * time.Second, "0m"},
		{-time.Hour, "0m"},
	}
	for _, one := range cases {
		t.Run(one.want, func(t *testing.T) {
			mustMatch(t, formatDuration(one.duration), one.want)
		})
	}
}
