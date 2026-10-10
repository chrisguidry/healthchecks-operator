package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// dnsAttempts is how many times the probe sends a question over UDP
// before it gives up. UDP drops packets now and then, and one lost
// packet is not a broken server.
const dnsAttempts = 3

// dnsTypes are the record types a query can ask for, by name.
var dnsTypes = map[string]dnsmessage.Type{
	"A":     dnsmessage.TypeA,
	"AAAA":  dnsmessage.TypeAAAA,
	"CNAME": dnsmessage.TypeCNAME,
	"NS":    dnsmessage.TypeNS,
	"PTR":   dnsmessage.TypePTR,
	"TXT":   dnsmessage.TypeTXT,
}

// rcodeNames are the response codes by the names dig prints, which is
// how a person reads them.
var rcodeNames = map[dnsmessage.RCode]string{
	dnsmessage.RCodeFormatError:    "FORMERR",
	dnsmessage.RCodeServerFailure:  "SERVFAIL",
	dnsmessage.RCodeNameError:      "NXDOMAIN",
	dnsmessage.RCodeNotImplemented: "NOTIMP",
	dnsmessage.RCodeRefused:        "REFUSED",
}

// dnsQuery is one query as the probe asks it: the question built, the
// server with its port, and the expected answers in the form the probe
// writes the answers it gets.
type dnsQuery struct {
	label    string
	server   string
	typeName string
	question dnsmessage.Question
	// want is nil when any answer of the type passes.
	want []string
}

type dnsProber struct {
	queries []dnsQuery
	timeout time.Duration
}

// newDNSProber builds the prober for a Check's dns block.
func newDNSProber(spec DNSProbe) (*dnsProber, error) {
	if len(spec.Queries) == 0 {
		return nil, errors.New("dns has no queries")
	}
	p := &dnsProber{timeout: probeTimeout}
	for _, query := range spec.Queries {
		built, err := buildDNSQuery(query)
		if err != nil {
			return nil, err
		}
		p.queries = append(p.queries, built)
	}
	return p, nil
}

func buildDNSQuery(query DNSQuery) (dnsQuery, error) {
	typeName := query.Type
	if typeName == "" {
		typeName = "A"
	}
	family, ok := dnsTypes[typeName]
	if !ok {
		return dnsQuery{}, fmt.Errorf("dns type %q is not one of A, AAAA, CNAME, NS, PTR, or TXT", query.Type)
	}
	server, ok := withDefaultPort(query.Server, "53")
	if !ok {
		return dnsQuery{}, fmt.Errorf("dns server %q is not host or host:port", query.Server)
	}
	lookup := query.Lookup
	if address, err := netip.ParseAddr(lookup); err == nil && family == dnsmessage.TypePTR {
		lookup = reverseName(address)
	}
	name, err := domainName(lookup)
	if err != nil {
		return dnsQuery{}, fmt.Errorf("dns lookup %q is not a domain name", query.Lookup)
	}
	built := dnsQuery{
		label:    query.Name,
		server:   server,
		typeName: typeName,
		question: dnsmessage.Question{Name: name, Type: family, Class: dnsmessage.ClassINET},
	}
	if built.label == "" {
		built.label = fmt.Sprintf("%s %s @%s", query.Lookup, typeName, query.Server)
	}
	for _, answer := range query.Expect.Answers {
		normal, err := normalAnswer(family, typeName, answer)
		if err != nil {
			return dnsQuery{}, err
		}
		built.want = append(built.want, normal)
	}
	return built, nil
}

// domainName makes a name fully qualified and checks the lengths that
// a name on the wire allows: 63 bytes to a label, 253 to the name.
func domainName(lookup string) (dnsmessage.Name, error) {
	trimmed := strings.TrimSuffix(lookup, ".")
	if trimmed == "" || len(trimmed) > 253 {
		return dnsmessage.Name{}, errors.New("bad length")
	}
	for label := range strings.SplitSeq(trimmed, ".") {
		if label == "" || len(label) > 63 {
			return dnsmessage.Name{}, errors.New("bad label")
		}
	}
	return dnsmessage.NewName(trimmed + ".")
}

// reverseName is the name a PTR record for address lives under, the
// name dig -x asks for.
func reverseName(address netip.Addr) string {
	if address.Is4() {
		octets := address.As4()
		return fmt.Sprintf("%d.%d.%d.%d.in-addr.arpa.", octets[3], octets[2], octets[1], octets[0])
	}
	const hex = "0123456789abcdef"
	var name strings.Builder
	bytes := address.As16()
	for i := len(bytes) - 1; i >= 0; i-- {
		name.WriteByte(hex[bytes[i]&0x0f])
		name.WriteByte('.')
		name.WriteByte(hex[bytes[i]>>4])
		name.WriteByte('.')
	}
	name.WriteString("ip6.arpa.")
	return name.String()
}

// normalAnswer writes an expected answer the way the probe writes the
// answers it gets: an address in its standard spelling, a name in lower
// case with no final dot, and text as it is.
func normalAnswer(family dnsmessage.Type, typeName, answer string) (string, error) {
	switch family {
	case dnsmessage.TypeA, dnsmessage.TypeAAAA:
		address, err := netip.ParseAddr(answer)
		if err != nil || address.Is4() != (family == dnsmessage.TypeA) {
			return "", fmt.Errorf("dns answer %q is not an %s address", answer, typeName)
		}
		return address.String(), nil
	case dnsmessage.TypeTXT:
		return answer, nil
	default:
		return strings.ToLower(strings.TrimSuffix(answer, ".")), nil
	}
}

// probe asks every question in order, past any that fail, so the
// reason shows which servers or names fail and which still answer.
func (p *dnsProber) probe(ctx context.Context) probeResult {
	var failed []string
	for _, query := range p.queries {
		if problem := p.ask(ctx, query); problem != "" {
			failed = append(failed, query.label+": "+problem)
		}
	}
	return resultOf(failed, len(p.queries), "queries")
}

// ask asks one question and returns what is wrong with the answer, or
// an empty string when it meets the expectations.
func (p *dnsProber) ask(ctx context.Context, query dnsQuery) string {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	id := uint16(rand.Uint32())
	message, err := (&dnsmessage.Message{
		Header:    dnsmessage.Header{ID: id, RecursionDesired: true},
		Questions: []dnsmessage.Question{query.question},
	}).Pack()
	if err != nil {
		return err.Error()
	}
	reply, err := p.exchangeUDP(ctx, query.server, message, id)
	if err == nil && truncated(reply) {
		reply, err = p.exchangeTCP(ctx, query.server, message)
	}
	if err != nil {
		return p.describeError(err)
	}
	return compareAnswers(query, reply)
}

// exchangeUDP sends the question and waits for the reply with its id,
// sending again after each third of the timeout. A reply with another
// id is a late answer to some other question, and the probe reads past
// it.
func (p *dnsProber) exchangeUDP(ctx context.Context, server string, message []byte, id uint16) ([]byte, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "udp", server)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	deadline, _ := ctx.Deadline()
	wait := p.timeout / dnsAttempts
	buffer := make([]byte, 65535)
	for range dnsAttempts {
		if _, err := conn.Write(message); err != nil {
			return nil, err
		}
		attemptEnds := time.Now().Add(wait)
		if deadline.Before(attemptEnds) {
			attemptEnds = deadline
		}
		_ = conn.SetReadDeadline(attemptEnds)
		for {
			n, err := conn.Read(buffer)
			if errors.Is(err, os.ErrDeadlineExceeded) {
				break
			}
			if err != nil {
				return nil, err
			}
			var header dnsmessage.Parser
			got, err := header.Start(buffer[:n])
			if err == nil && got.Response && got.ID == id {
				return buffer[:n], nil
			}
		}
	}
	return nil, context.DeadlineExceeded
}

// exchangeTCP asks again over TCP, for an answer too large for UDP.
// Each message on a TCP stream carries its length in two bytes first.
func (p *dnsProber) exchangeTCP(ctx context.Context, server string, message []byte) ([]byte, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", server)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	framed := binary.BigEndian.AppendUint16(nil, uint16(len(message)))
	if _, err := conn.Write(append(framed, message...)); err != nil {
		return nil, err
	}
	var length uint16
	if err := binary.Read(conn, binary.BigEndian, &length); err != nil {
		return nil, err
	}
	reply := make([]byte, length)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return nil, err
	}
	return reply, nil
}

func truncated(reply []byte) bool {
	var parser dnsmessage.Parser
	header, err := parser.Start(reply)
	return err == nil && header.Truncated
}

// compareAnswers reads the records of the question's type from the
// answer section, and compares them with the expected set. A CNAME on
// the way to an address is not an answer to an A question, so records
// of other types are skipped.
func compareAnswers(query dnsQuery, reply []byte) string {
	var parser dnsmessage.Parser
	header, err := parser.Start(reply)
	if err != nil {
		return "unreadable answer: " + err.Error()
	}
	if header.RCode != dnsmessage.RCodeSuccess {
		if name, ok := rcodeNames[header.RCode]; ok {
			return name
		}
		return fmt.Sprintf("rcode %d", header.RCode)
	}
	got, err := readAnswers(&parser, query.question.Type)
	if err != nil {
		return "unreadable answer: " + err.Error()
	}
	if len(got) == 0 {
		if query.want == nil {
			return fmt.Sprintf("no %s records", query.typeName)
		}
		return fmt.Sprintf("no %s records, want %s", query.typeName, listAnswers(query.question.Type, query.want))
	}
	if query.want == nil {
		return ""
	}
	var problems []string
	if missing := without(query.want, got); len(missing) > 0 {
		problems = append(problems, "missing "+listAnswers(query.question.Type, missing))
	}
	if unexpected := without(got, query.want); len(unexpected) > 0 {
		problems = append(problems, "unexpected "+listAnswers(query.question.Type, unexpected))
	}
	return strings.Join(problems, "; ")
}

func readAnswers(parser *dnsmessage.Parser, family dnsmessage.Type) ([]string, error) {
	if err := parser.SkipAllQuestions(); err != nil {
		return nil, err
	}
	var answers []string
	for {
		header, err := parser.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			return answers, nil
		}
		if err != nil {
			return nil, err
		}
		if header.Type != family || header.Class != dnsmessage.ClassINET {
			if err := parser.SkipAnswer(); err != nil {
				return nil, err
			}
			continue
		}
		answer, err := readAnswer(parser, family)
		if err != nil {
			return nil, err
		}
		answers = append(answers, answer)
	}
}

// readAnswer reads one record's body in the form normalAnswer writes an
// expected answer.
func readAnswer(parser *dnsmessage.Parser, family dnsmessage.Type) (string, error) {
	switch family {
	case dnsmessage.TypeA:
		record, err := parser.AResource()
		return netip.AddrFrom4(record.A).String(), err
	case dnsmessage.TypeAAAA:
		record, err := parser.AAAAResource()
		return netip.AddrFrom16(record.AAAA).String(), err
	case dnsmessage.TypeCNAME:
		record, err := parser.CNAMEResource()
		return normalName(record.CNAME), err
	case dnsmessage.TypeNS:
		record, err := parser.NSResource()
		return normalName(record.NS), err
	case dnsmessage.TypePTR:
		record, err := parser.PTRResource()
		return normalName(record.PTR), err
	default:
		record, err := parser.TXTResource()
		return strings.Join(record.TXT, ""), err
	}
}

func normalName(name dnsmessage.Name) string {
	return strings.ToLower(strings.TrimSuffix(name.String(), "."))
}

// without returns the entries of from that are not in other, sorted.
func without(from, other []string) []string {
	var left []string
	for _, entry := range from {
		if !slices.Contains(other, entry) {
			left = append(left, entry)
		}
	}
	slices.Sort(left)
	return left
}

// listAnswers writes answers for a reason, quoting text, which may hold
// spaces and commas of its own.
func listAnswers(family dnsmessage.Type, answers []string) string {
	written := make([]string, len(answers))
	for i, answer := range answers {
		written[i] = answer
		if family == dnsmessage.TypeTXT {
			written[i] = fmt.Sprintf("%q", answer)
		}
	}
	return strings.Join(written, ", ")
}

// describeError drops the "read udp <address>->" that net puts in
// front of its errors, because the reason already names the server.
func (p *dnsProber) describeError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Sprintf("no answer within %s", p.timeout)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "the server closed the TCP connection before it answered"
	}
	if dnsError, ok := errors.AsType[*net.DNSError](err); ok {
		return fmt.Sprintf("lookup %s: %s", dnsError.Name, dnsError.Err)
	}
	if opError, ok := errors.AsType[*net.OpError](err); ok {
		return opError.Err.Error()
	}
	return err.Error()
}
