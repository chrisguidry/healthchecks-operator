package main

import (
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// dnsAnswer is how the fake server answers one question.
type dnsAnswer struct {
	rcode   dnsmessage.RCode
	records []dnsmessage.Resource
	// truncated answers over UDP with the TC bit and no records, so the
	// probe must ask again over TCP, which gets the records.
	truncated bool
	// silent never answers.
	silent bool
	// hangUp answers over UDP with the TC bit, then closes the TCP
	// connection without an answer.
	hangUp bool
	// garbled answers with a header and no question, though the header
	// counts one.
	garbled bool
}

func dnsName(name string) dnsmessage.Name {
	return dnsmessage.MustNewName(name)
}

func aRecord(name, address string) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsName(name), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
		Body:   &dnsmessage.AResource{A: netip.MustParseAddr(address).As4()},
	}
}

func aaaaRecord(name, address string) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsName(name), Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET},
		Body:   &dnsmessage.AAAAResource{AAAA: netip.MustParseAddr(address).As16()},
	}
}

func cnameRecord(name, target string) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsName(name), Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET},
		Body:   &dnsmessage.CNAMEResource{CNAME: dnsName(target)},
	}
}

func nsRecord(name, server string) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsName(name), Type: dnsmessage.TypeNS, Class: dnsmessage.ClassINET},
		Body:   &dnsmessage.NSResource{NS: dnsName(server)},
	}
}

func ptrRecord(name, target string) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsName(name), Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET},
		Body:   &dnsmessage.PTRResource{PTR: dnsName(target)},
	}
}

func txtRecord(name string, texts ...string) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsName(name), Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET},
		Body:   &dnsmessage.TXTResource{TXT: texts},
	}
}

type dnsQuestion struct {
	name   string
	family dnsmessage.Type
}

// exampleZone is what the fake server knows. A question it does not
// list gets NOERROR with no records.
var exampleZone = map[dnsQuestion]dnsAnswer{
	{"www.example.com.", dnsmessage.TypeA}: {records: []dnsmessage.Resource{
		aRecord("www.example.com.", "192.0.2.1"), aRecord("www.example.com.", "192.0.2.2")}},
	{"www.example.com.", dnsmessage.TypeAAAA}: {records: []dnsmessage.Resource{
		aaaaRecord("www.example.com.", "2001:db8::1")}},
	{"alias.example.com.", dnsmessage.TypeA}: {records: []dnsmessage.Resource{
		cnameRecord("alias.example.com.", "www.example.com."), aRecord("www.example.com.", "192.0.2.1")}},
	{"alias.example.com.", dnsmessage.TypeCNAME}: {records: []dnsmessage.Resource{
		cnameRecord("alias.example.com.", "www.example.com.")}},
	{"example.com.", dnsmessage.TypeNS}: {records: []dnsmessage.Resource{
		nsRecord("example.com.", "ns1.example.com."), nsRecord("example.com.", "ns2.example.com.")}},
	{"example.com.", dnsmessage.TypeTXT}: {records: []dnsmessage.Resource{
		txtRecord("example.com.", "v=spf1 ", "-all")}},
	{"1.2.0.192.in-addr.arpa.", dnsmessage.TypePTR}: {records: []dnsmessage.Resource{
		ptrRecord("1.2.0.192.in-addr.arpa.", "www.example.com.")}},
	{"1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa.", dnsmessage.TypePTR}: {records: []dnsmessage.Resource{
		ptrRecord("1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa.", "www.example.com.")}},
	{"hangup.example.com.", dnsmessage.TypeA}:  {truncated: true, hangUp: true},
	{"garbled.example.com.", dnsmessage.TypeA}: {garbled: true},
	{"notauth.example.com.", dnsmessage.TypeA}: {rcode: dnsmessage.RCode(9)},
	{"big.example.com.", dnsmessage.TypeA}: {truncated: true, records: []dnsmessage.Resource{
		aRecord("big.example.com.", "192.0.2.9")}},
	{"missing.example.com.", dnsmessage.TypeA}:  {rcode: dnsmessage.RCodeNameError},
	{"broken.example.com.", dnsmessage.TypeA}:   {rcode: dnsmessage.RCodeServerFailure},
	{"silent.example.com.", dnsmessage.TypeA}:   {silent: true},
	{"stranger.example.com.", dnsmessage.TypeA}: {},
}

// answerFor builds the reply to one query, or nil when the server stays
// silent. overUDP is false for a query that came over TCP.
func answerFor(t *testing.T, query []byte, overUDP bool) []byte {
	var parser dnsmessage.Parser
	header, err := parser.Start(query)
	if err != nil {
		return nil
	}
	question, err := parser.Question()
	if err != nil {
		return nil
	}
	answer := exampleZone[dnsQuestion{question.Name.String(), question.Type}]
	if answer.silent || (answer.hangUp && !overUDP) {
		return nil
	}
	if answer.garbled {
		reply := make([]byte, 12)
		binary.BigEndian.PutUint16(reply[0:], header.ID)
		binary.BigEndian.PutUint16(reply[2:], 1<<15)
		binary.BigEndian.PutUint16(reply[4:], 1)
		return reply
	}
	truncated := answer.truncated && overUDP
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID: header.ID, Response: true, RecursionDesired: header.RecursionDesired,
		RCode: answer.rcode, Truncated: truncated,
	})
	mustSucceed(t, builder.StartQuestions())
	mustSucceed(t, builder.Question(question))
	mustSucceed(t, builder.StartAnswers())
	if !truncated {
		for _, record := range answer.records {
			switch body := record.Body.(type) {
			case *dnsmessage.AResource:
				mustSucceed(t, builder.AResource(record.Header, *body))
			case *dnsmessage.AAAAResource:
				mustSucceed(t, builder.AAAAResource(record.Header, *body))
			case *dnsmessage.CNAMEResource:
				mustSucceed(t, builder.CNAMEResource(record.Header, *body))
			case *dnsmessage.NSResource:
				mustSucceed(t, builder.NSResource(record.Header, *body))
			case *dnsmessage.PTRResource:
				mustSucceed(t, builder.PTRResource(record.Header, *body))
			case *dnsmessage.TXTResource:
				mustSucceed(t, builder.TXTResource(record.Header, *body))
			}
		}
	}
	reply, err := builder.Finish()
	mustSucceed(t, err)
	return reply
}

// listenUDPAndTCP opens one local port number for both UDP and TCP. A
// free UDP port can have its number taken for TCP, so it tries again
// on a new port when that happens.
func listenUDPAndTCP(t *testing.T) (net.PacketConn, net.Listener) {
	t.Helper()
	for range 10 {
		packets, err := net.ListenPacket("udp", "127.0.0.1:0")
		mustSucceed(t, err)
		streams, err := net.Listen("tcp", packets.LocalAddr().String())
		if err == nil {
			t.Cleanup(func() { _ = packets.Close(); _ = streams.Close() })
			return packets, streams
		}
		_ = packets.Close()
	}
	t.Fatal("found no port free for both UDP and TCP")
	return nil, nil
}

// serveDNS answers exampleZone over UDP and TCP on one local port, and
// returns the port's address.
func serveDNS(t *testing.T) string {
	t.Helper()
	packets, streams := listenUDPAndTCP(t)
	address := packets.LocalAddr().String()
	go func() {
		buffer := make([]byte, 65535)
		for {
			n, from, err := packets.ReadFrom(buffer)
			if err != nil {
				return
			}
			if reply := answerFor(t, buffer[:n], true); reply != nil {
				_, _ = packets.WriteTo(reply, from)
			}
		}
	}()
	go func() {
		for {
			conn, err := streams.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				var length uint16
				if binary.Read(conn, binary.BigEndian, &length) != nil {
					return
				}
				query := make([]byte, length)
				if _, err := io.ReadFull(conn, query); err != nil {
					return
				}
				reply := answerFor(t, query, false)
				if reply == nil {
					return
				}
				_ = binary.Write(conn, binary.BigEndian, uint16(len(reply)))
				_, _ = conn.Write(reply)
			}()
		}
	}()
	return address
}

// dnsProbeAt points every query at server.
func dnsProbeAt(server string, queries []DNSQuery) DNSProbe {
	spec := DNSProbe{Interval: "5m"}
	for _, query := range queries {
		query.Server = server
		spec.Queries = append(spec.Queries, query)
	}
	return spec
}

func TestDNSProbeResults(t *testing.T) {
	cases := []struct {
		name    string
		queries []DNSQuery
		reason  string
	}{
		{"any answer passes without expectations",
			[]DNSQuery{{Lookup: "www.example.com"}}, ""},
		{"the answers match",
			[]DNSQuery{{Lookup: "www.example.com", Expect: DNSExpectation{Answers: []string{"192.0.2.2", "192.0.2.1"}}}}, ""},
		{"the answers differ",
			[]DNSQuery{{Lookup: "www.example.com", Expect: DNSExpectation{Answers: []string{"192.0.2.1", "192.0.2.3"}}}},
			"www.example.com A @{server}: missing 192.0.2.3; unexpected 192.0.2.2"},
		{"an address matches in any of its spellings",
			[]DNSQuery{{Lookup: "www.example.com", Type: "AAAA", Expect: DNSExpectation{Answers: []string{"2001:0db8::0001"}}}}, ""},
		{"a name matches in any case, with or without its final dot",
			[]DNSQuery{{Lookup: "alias.example.com", Type: "CNAME", Expect: DNSExpectation{Answers: []string{"WWW.Example.com."}}}}, ""},
		{"an A lookup through a CNAME answers with the addresses only",
			[]DNSQuery{{Lookup: "alias.example.com.", Expect: DNSExpectation{Answers: []string{"192.0.2.1"}}}}, ""},
		{"an NS lookup",
			[]DNSQuery{{Lookup: "example.com", Type: "NS", Expect: DNSExpectation{Answers: []string{"ns1.example.com", "ns2.example.com"}}}}, ""},
		{"a PTR lookup by address",
			[]DNSQuery{{Lookup: "192.0.2.1", Type: "PTR", Expect: DNSExpectation{Answers: []string{"www.example.com"}}}}, ""},
		{"a PTR lookup by IPv6 address",
			[]DNSQuery{{Lookup: "2001:db8::1", Type: "PTR", Expect: DNSExpectation{Answers: []string{"www.example.com"}}}}, ""},
		{"a TXT record's strings are joined",
			[]DNSQuery{{Lookup: "example.com", Type: "TXT", Expect: DNSExpectation{Answers: []string{"v=spf1 -all"}}}}, ""},
		{"a truncated answer is asked again over TCP",
			[]DNSQuery{{Lookup: "big.example.com", Expect: DNSExpectation{Answers: []string{"192.0.2.9"}}}}, ""},
		{"a name that does not exist",
			[]DNSQuery{{Lookup: "missing.example.com"}},
			"missing.example.com A @{server}: NXDOMAIN"},
		{"a response code that has no name here",
			[]DNSQuery{{Lookup: "notauth.example.com"}},
			"notauth.example.com A @{server}: rcode 9"},
		{"a TCP connection that closes before the answer",
			[]DNSQuery{{Lookup: "hangup.example.com"}},
			"hangup.example.com A @{server}: the server closed the TCP connection before it answered"},
		{"an answer that does not parse",
			[]DNSQuery{{Lookup: "garbled.example.com"}},
			"garbled.example.com A @{server}: unreadable answer: {question}"},
		{"a server failure",
			[]DNSQuery{{Lookup: "broken.example.com"}},
			"broken.example.com A @{server}: SERVFAIL"},
		{"a name with no records of the type",
			[]DNSQuery{{Lookup: "stranger.example.com"}},
			"stranger.example.com A @{server}: no A records"},
		{"a name with no records of the type, when answers are expected",
			[]DNSQuery{{Lookup: "stranger.example.com", Expect: DNSExpectation{Answers: []string{"192.0.2.1"}}}},
			"stranger.example.com A @{server}: no A records, want 192.0.2.1"},
		{"a named query is reported by its name",
			[]DNSQuery{{Name: "the zone's mail policy", Lookup: "example.com", Type: "TXT", Expect: DNSExpectation{Answers: []string{"v=spf1 ~all"}}}},
			`the zone's mail policy: missing "v=spf1 ~all"; unexpected "v=spf1 -all"`},
		{"every failed query is reported",
			[]DNSQuery{
				{Lookup: "missing.example.com"},
				{Lookup: "www.example.com"},
				{Lookup: "broken.example.com"}},
			"2 of 3 queries failed\n" +
				"missing.example.com A @{server}: NXDOMAIN\n" +
				"broken.example.com A @{server}: SERVFAIL"},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			server := serveDNS(t)
			p, err := newDNSProber(dnsProbeAt(server, one.queries))
			mustSucceed(t, err)

			result := p.probe(t.Context())

			reason := strings.NewReplacer("{server}", server, "{question}", garbledError(t)).Replace(one.reason)
			mustMatch(t, result, probeResult{passed: one.reason == "", reason: reason})
		})
	}
}

// garbledError is what dnsmessage says about an answer whose header
// counts a question that is not there.
func garbledError(t *testing.T) string {
	t.Helper()
	var parser dnsmessage.Parser
	_, err := parser.Start([]byte{0, 0, 0x80, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	mustSucceed(t, err)
	return parser.SkipAllQuestions().Error()
}

func TestDNSProbeFailsWhenTheServerCannotBeReached(t *testing.T) {
	cases := []struct {
		name   string
		server string
		reason string
	}{
		{"no server listens on the port", "{closed}", "{closed}: read: connection refused"},
		// Go's resolver refuses every .onion name without asking DNS.
		{"the server's name does not resolve", "no-such-host.onion", "no-such-host.onion: lookup no-such-host.onion: no such host"},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			closed := closedUDPAddress(t)
			server := strings.ReplaceAll(one.server, "{closed}", closed)
			p, err := newDNSProber(DNSProbe{Interval: "5m", Queries: []DNSQuery{{Name: server, Server: server, Lookup: "www.example.com"}}})
			mustSucceed(t, err)

			result := p.probe(t.Context())

			mustMatch(t, result, probeResult{reason: strings.ReplaceAll(one.reason, "{closed}", closed)})
		})
	}
}

// closedUDPAddress is a local UDP port that nothing listens on, so the
// machine answers a packet to it with an ICMP refusal.
func closedUDPAddress(t *testing.T) string {
	t.Helper()
	packets, err := net.ListenPacket("udp", "127.0.0.1:0")
	mustSucceed(t, err)
	address := packets.LocalAddr().String()
	mustSucceed(t, packets.Close())
	return address
}

func TestDNSProbeGivesUpOnAServerThatDoesNotAnswer(t *testing.T) {
	server := serveDNS(t)
	p, err := newDNSProber(dnsProbeAt(server, []DNSQuery{{Lookup: "silent.example.com"}}))
	mustSucceed(t, err)
	p.timeout = 50 * time.Millisecond

	result := p.probe(t.Context())

	mustMatch(t, result, probeResult{reason: "silent.example.com A @" + server + ": no answer within 50ms"})
}

func TestDNSProberDefaultsThePortTo53(t *testing.T) {
	p, err := newDNSProber(DNSProbe{Interval: "5m", Queries: []DNSQuery{{Server: "192.0.2.53", Lookup: "example.com"}}})
	mustSucceed(t, err)

	mustMatch(t, p.queries[0].server, "192.0.2.53:53")
}

func TestDNSProberRejectsASpecItCannotAsk(t *testing.T) {
	cases := []struct {
		name    string
		queries []DNSQuery
		want    string
	}{
		{"no queries", nil, "dns has no queries"},
		{"a type it does not ask", []DNSQuery{{Server: "192.0.2.53", Lookup: "example.com", Type: "MX"}},
			`dns type "MX" is not one of A, AAAA, CNAME, NS, PTR, or TXT`},
		{"a lookup with no name", []DNSQuery{{Server: "192.0.2.53", Lookup: "."}},
			`dns lookup "." is not a domain name`},
		{"a name too long to ask", []DNSQuery{{Server: "192.0.2.53", Lookup: strings.Repeat("a", 64) + ".example.com"}},
			`dns lookup "` + strings.Repeat("a", 64) + `.example.com" is not a domain name`},
		{"an expected address that is not one", []DNSQuery{{Server: "192.0.2.53", Lookup: "example.com", Expect: DNSExpectation{Answers: []string{"192.0.2"}}}},
			`dns answer "192.0.2" is not an A address`},
		{"a server that is not host or host:port", []DNSQuery{{Server: "192.0.2.53:53:53", Lookup: "example.com"}},
			`dns server "192.0.2.53:53:53" is not host or host:port`},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			_, err := newDNSProber(DNSProbe{Interval: "5m", Queries: one.queries})

			mustMatch(t, err.Error(), one.want)
		})
	}
}
