package main

import (
	"net"
	"strings"
	"testing"
	"time"
)

// serveTCP accepts connections on a local port and hands each one to
// handle, and returns the port's address.
func serveTCP(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	mustSucceed(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				handle(conn)
			}()
		}
	}()
	return listener.Addr().String()
}

type tcpProbeHarness struct {
	// addresses replaces {banner}, {silent}, and {closed} in a case.
	addresses *strings.Replacer
}

// startTCPTargets serves a port that sends an SSH banner, a port that
// accepts and sends nothing, and a port that refuses.
func startTCPTargets(t *testing.T) *tcpProbeHarness {
	t.Helper()
	banner := serveTCP(t, func(conn net.Conn) {
		_, _ = conn.Write([]byte("SSH-2.0-example\r\n"))
	})
	silent := serveTCP(t, func(conn net.Conn) {
		buffer := make([]byte, 1)
		_, _ = conn.Read(buffer)
	})
	return &tcpProbeHarness{addresses: strings.NewReplacer(
		"{banner}", banner, "{silent}", silent, "{closed}", closedAddress(t))}
}

func (h *tcpProbeHarness) probeSpec(connections []TCPConnection) TCPProbe {
	spec := TCPProbe{Interval: "5m"}
	for _, connection := range connections {
		connection.Address = h.addresses.Replace(connection.Address)
		spec.Connections = append(spec.Connections, connection)
	}
	return spec
}

func TestTCPProbeResults(t *testing.T) {
	cases := []struct {
		name        string
		connections []TCPConnection
		reason      string
	}{
		{"a port that accepts passes without expectations",
			[]TCPConnection{{Address: "{silent}"}}, ""},
		{"the banner contains the text",
			[]TCPConnection{{Address: "{banner}", Expect: TCPExpectation{BannerContains: "SSH-2.0-"}}}, ""},
		{"the banner lacks the text",
			[]TCPConnection{{Address: "{banner}", Expect: TCPExpectation{BannerContains: "SMTP"}}},
			`{banner}: banner does not contain "SMTP", got "SSH-2.0-example\r\n"`},
		{"a port that refuses",
			[]TCPConnection{{Address: "{closed}"}},
			"{closed}: connect: connection refused"},
		{"a port expected closed refuses",
			[]TCPConnection{{Address: "{closed}", Expect: TCPExpectation{Closed: true}}}, ""},
		{"a port expected closed accepts",
			[]TCPConnection{{Address: "{silent}", Expect: TCPExpectation{Closed: true}}},
			"{silent}: connected, want the port closed"},
		{"a named connection is reported by its name",
			[]TCPConnection{{Name: "ssh on the proxy", Address: "{closed}"}},
			"ssh on the proxy: connect: connection refused"},
		{"every failed connection is reported",
			[]TCPConnection{
				{Address: "{closed}"},
				{Address: "{banner}", Expect: TCPExpectation{BannerContains: "SSH-2.0-"}},
				{Address: "{silent}", Expect: TCPExpectation{Closed: true}}},
			"2 of 3 connections failed\n" +
				"{closed}: connect: connection refused\n" +
				"{silent}: connected, want the port closed"},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			h := startTCPTargets(t)
			p, err := newTCPProber(h.probeSpec(one.connections))
			mustSucceed(t, err)

			result := p.probe(t.Context())

			mustMatch(t, result, probeResult{passed: one.reason == "", reason: h.addresses.Replace(one.reason)})
		})
	}
}

func TestTCPProbeGivesUpOnAPortThatSendsNoBanner(t *testing.T) {
	h := startTCPTargets(t)
	p, err := newTCPProber(h.probeSpec([]TCPConnection{{Address: "{silent}", Expect: TCPExpectation{BannerContains: "SSH-"}}}))
	mustSucceed(t, err)
	p.timeout = 50 * time.Millisecond

	result := p.probe(t.Context())

	mustMatch(t, result, probeResult{reason: h.addresses.Replace("{silent}: no banner within 50ms")})
}

// A name that does not resolve is a fault in the spec or in DNS, not a
// closed port, so it fails a connection that expects the port closed.
// Go's resolver refuses every .onion name without asking DNS, as RFC
// 7686 asks, so the test needs no DNS server.
func TestTCPProbeFailsAClosedExpectationOnANameThatDoesNotResolve(t *testing.T) {
	p, err := newTCPProber(TCPProbe{Interval: "5m", Connections: []TCPConnection{
		{Address: "no-such-host.onion:22", Expect: TCPExpectation{Closed: true}}}})
	mustSucceed(t, err)

	result := p.probe(t.Context())

	mustMatch(t, result, probeResult{reason: "no-such-host.onion:22: lookup no-such-host.onion: no such host"})
}

func TestTCPProberRejectsASpecItCannotDial(t *testing.T) {
	cases := []struct {
		name        string
		connections []TCPConnection
		want        string
	}{
		{"no connections", nil, "tcp has no connections"},
		{"no port", []TCPConnection{{Address: "example.com"}}, `tcp address "example.com" is not host:port`},
		{"an empty port", []TCPConnection{{Address: "example.com:"}}, `tcp address "example.com:" is not host:port`},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			_, err := newTCPProber(TCPProbe{Interval: "5m", Connections: one.connections})

			mustMatch(t, err.Error(), one.want)
		})
	}
}
