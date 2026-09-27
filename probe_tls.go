package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

type tlsProber struct {
	// host is host:port, and serverName is the host part, which the
	// handshake sends as SNI and verifies the certificate against.
	host         string
	serverName   string
	minRemaining time.Duration
	// roots is nil for the system roots.
	roots   *x509.CertPool
	now     func() time.Time
	timeout time.Duration
}

// newTLSProber builds the prober for a Check's tls block.
func newTLSProber(spec TLSProbe) (prober, error) {
	return newTLSProberWith(spec, nil, time.Now)
}

// withTLSPort adds port 443 to a host that names no port, because 443 is
// the port nearly every certificate worth watching is served on. A bare
// IPv6 address has colons of its own, so it is recognized as an address
// before the port is added.
func withTLSPort(host string) (string, error) {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host, nil
	}
	if !strings.Contains(host, ":") || net.ParseIP(host) != nil {
		return net.JoinHostPort(host, "443"), nil
	}
	return "", fmt.Errorf("tls.host %q is not host or host:port", host)
}

// newTLSProberWith trusts roots in place of the system roots when roots
// is not nil, and reads the time from now, so a test controls both the
// certificate and the moment it is checked.
func newTLSProberWith(spec TLSProbe, roots *x509.CertPool, now func() time.Time) (*tlsProber, error) {
	host, err := withTLSPort(spec.Host)
	if err != nil {
		return nil, err
	}
	serverName, _, _ := net.SplitHostPort(host)
	minRemaining, err := time.ParseDuration(spec.MinRemaining)
	if err != nil {
		return nil, fmt.Errorf("tls.minRemaining: %w", err)
	}
	if minRemaining <= 0 {
		return nil, fmt.Errorf("tls.minRemaining %s is not positive", spec.MinRemaining)
	}
	return &tlsProber{
		host:         host,
		serverName:   serverName,
		minRemaining: minRemaining,
		roots:        roots,
		now:          now,
		timeout:      probeTimeout,
	}, nil
}

// probe completes a handshake that verifies the chain and the name, the
// same checks a browser makes, then compares the leaf's expiry with
// minRemaining.
func (p *tlsProber) probe(ctx context.Context) probeResult {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	now := p.now()
	dialer := &tls.Dialer{Config: &tls.Config{
		ServerName: p.serverName,
		RootCAs:    p.roots,
		// Verification reads the same clock as the expiry check, so the
		// two cannot disagree about whether the certificate expired.
		Time: func() time.Time { return now },
	}}
	conn, err := dialer.DialContext(ctx, "tcp", p.host)
	if errors.Is(err, context.DeadlineExceeded) {
		return probeResult{reason: fmt.Sprintf("%s: no answer within %s", p.host, p.timeout)}
	}
	if err != nil {
		return probeResult{reason: p.host + ": " + err.Error()}
	}
	defer func() { _ = conn.Close() }()
	leaf := conn.(*tls.Conn).ConnectionState().PeerCertificates[0]
	left := leaf.NotAfter.Sub(now)
	if left < p.minRemaining {
		return probeResult{reason: fmt.Sprintf("%s: certificate expires %s, %s left, want at least %s",
			p.host, leaf.NotAfter.UTC().Format(time.RFC3339), formatDuration(left), formatDuration(p.minRemaining))}
	}
	return probeResult{passed: true}
}

// formatDuration writes a certificate's lifetime in days, hours, and
// minutes, such as 6d or 1d12h, because time.Duration.String writes
// 336h0m0s for two weeks. It drops the seconds.
func formatDuration(d time.Duration) string {
	d = d.Truncate(time.Minute)
	if d <= 0 {
		return "0m"
	}
	var out strings.Builder
	for _, unit := range []struct {
		size time.Duration
		name string
	}{{24 * time.Hour, "d"}, {time.Hour, "h"}, {time.Minute, "m"}} {
		if count := d / unit.size; count > 0 {
			fmt.Fprintf(&out, "%d%s", count, unit.name)
			d -= count * unit.size
		}
	}
	return out.String()
}
