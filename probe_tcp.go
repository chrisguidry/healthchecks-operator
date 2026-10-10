package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

// bannerLimit is how much of a server's first bytes the probe reads to
// look for bannerContains. A protocol greeting is one short line, and a
// server that streams without end cannot hold a run or its memory.
const bannerLimit = 4096

// bannerShown is how much of a banner a failure reason quotes, enough
// to tell one server's greeting from another's.
const bannerShown = 120

type tcpProber struct {
	connections []TCPConnection
	timeout     time.Duration
}

// newTCPProber builds the prober for a Check's tcp block.
func newTCPProber(spec TCPProbe) (*tcpProber, error) {
	if len(spec.Connections) == 0 {
		return nil, errors.New("tcp has no connections")
	}
	for _, connection := range spec.Connections {
		if _, port, err := net.SplitHostPort(connection.Address); err != nil || port == "" {
			return nil, fmt.Errorf("tcp address %q is not host:port", connection.Address)
		}
	}
	return &tcpProber{connections: spec.Connections, timeout: probeTimeout}, nil
}

// probe opens every connection in order, past any that fail, so the
// reason shows which ports fail and which still work.
func (p *tcpProber) probe(ctx context.Context) probeResult {
	var failed []string
	for _, connection := range p.connections {
		if problem := p.open(ctx, connection); problem != "" {
			failed = append(failed, connectionLabel(connection)+": "+problem)
		}
	}
	return resultOf(failed, len(p.connections), "connections")
}

// connectionLabel names a connection in a reason: by its name when it
// has one, and by its address otherwise.
func connectionLabel(connection TCPConnection) string {
	if connection.Name != "" {
		return connection.Name
	}
	return connection.Address
}

// open makes one connection and returns what is wrong with it, or an
// empty string when it meets its expectations.
func (p *tcpProber) open(ctx context.Context, connection TCPConnection) string {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", connection.Address)
	if connection.Expect.Closed {
		return p.compareClosed(err, conn)
	}
	if err != nil {
		return p.describeError(err)
	}
	defer func() { _ = conn.Close() }()
	if connection.Expect.BannerContains == "" {
		return ""
	}
	deadline, _ := ctx.Deadline()
	_ = conn.SetReadDeadline(deadline)
	return p.compareBanner(conn, connection.Expect.BannerContains)
}

// compareClosed passes a port that refuses the connection or does not
// answer, the two ways a firewall keeps a port shut. A name that does
// not resolve fails, because it says nothing about the port.
func (p *tcpProber) compareClosed(err error, conn net.Conn) string {
	if err == nil {
		_ = conn.Close()
		return "connected, want the port closed"
	}
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return p.describeError(err)
	}
	return ""
}

// compareBanner reads the server's first bytes until they contain the
// text, the server stops sending, or the deadline passes.
func (p *tcpProber) compareBanner(conn net.Conn, contains string) string {
	var banner []byte
	buffer := make([]byte, 512)
	for len(banner) < bannerLimit {
		n, err := conn.Read(buffer)
		banner = append(banner, buffer[:n]...)
		if bytes.Contains(banner, []byte(contains)) {
			return ""
		}
		if err != nil {
			break
		}
	}
	if len(banner) == 0 {
		return fmt.Sprintf("no banner within %s", p.timeout)
	}
	return fmt.Sprintf("banner does not contain %q, got %q", contains, banner[:min(len(banner), bannerShown)])
}

// describeError drops the "dial tcp <address>:" that net puts in front
// of its errors, because the reason already starts with the address.
func (p *tcpProber) describeError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Sprintf("no answer within %s", p.timeout)
	}
	if opError, ok := errors.AsType[*net.OpError](err); ok {
		return opError.Err.Error()
	}
	return err.Error()
}
