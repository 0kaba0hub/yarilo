package main

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Row: one LMTP delivery, visible to every configured reader. The other rows
// compare a pair; this one asserts that a single arrival reaches IMAP, POP3 and
// JMAP at all — the agreement a user notices first, and the one a per-protocol
// check cannot make, because each protocol was asked about a different message.
//
// POP3 also reports the message's octet count, which is the one fact it shares
// with the other surfaces: LIST answers in octets and so does RFC822.SIZE, over
// the same stored message. That comparison is in this row rather than in a POP3
// pair of its own — see the note in checkConsistencyReaders on why POP3 gets no
// further rows.
func checkConsistencyReaders(user, pass string) error {
	marker := consistencyMarker("readers")
	if err := deliverConsistencyProbe(user, marker); err != nil {
		return err
	}

	imapSide, err := imapReadProbe(user, pass, marker)
	if err != nil {
		return fmt.Errorf("the delivery never reached imap: %w", err)
	}
	readers := []*reading{imapSide}

	if *flagJMAP && *flagJMAPUser != "" {
		jmapSide, err := jmapReadProbe(marker)
		if err != nil {
			return fmt.Errorf("the delivery never reached jmap: %w", err)
		}
		readers = append(readers, jmapSide)
	}
	if *flagPOP3S {
		pop3Side, err := pop3ReadProbe(user, pass, marker)
		if err != nil {
			return fmt.Errorf("the delivery never reached pop3: %w", err)
		}
		readers = append(readers, pop3Side)
	}

	// Every reader against the first: a message every surface agrees about is
	// the property, and comparing pairwise from one anchor names which reader
	// disagreed rather than reporting "the readers disagree".
	for _, r := range readers[1:] {
		row := fmt.Sprintf("one delivery seen by %s and %s", readers[0].surface, r.surface)
		if err := judgeRow(row, sharedFields(readers[0], r), r, defaultAllowances()); err != nil {
			return err
		}
	}
	return nil
}

// sharedFields narrows the anchor's reading to what the other surface can also
// report: POP3 has no notion of an EMAILID and does not decode subjects, so
// demanding them would refuse a reader for lacking a concept rather than for
// disagreeing. What survives is compared exactly.
func sharedFields(anchor, other *reading) *reading {
	out := newReading(anchor.surface)
	for name, v := range anchor.fields {
		if _, ok := other.fields[name]; ok {
			out.field(name, v)
		}
	}
	for name, v := range anchor.sets {
		if _, ok := other.sets[name]; ok {
			out.set(name, v)
		}
	}
	return out
}

// pop3ReadProbe finds the probe over POP3 and reports the facts POP3 has: the
// octet count, and the subject as the headers carry it.
//
// Why POP3 appears here and in no pair of its own: the only identity POP3
// exposes is UIDL, and RFC 1939 defines it as unique within one maildrop and
// persistent across sessions — explicitly not related to any other protocol's
// identifiers. Comparing UIDL to an IMAP UID would pin our own derivation as a
// cross-surface contract, which is the opposite of what this area is for: it
// would turn an implementation detail into a promise, and a legitimate change
// of derivation into a red gate. Size is different — octets of the same stored
// message are the same fact under both protocols — so that comparison is real
// and is made here.
func pop3ReadProbe(user, pass, marker string) (*reading, error) {
	addr := net.JoinHostPort(pop3Host(), *flagPOP3SPort)
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: *flagTimeout}, "tcp", addr,
		&tls.Config{ServerName: pop3Host(), InsecureSkipVerify: *flagInsecure}) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(*flagTimeout)) //nolint:errcheck
	p := &pop3Client{conn: conn, r: bufio.NewReader(conn)}
	if _, err := p.line(); err != nil {
		return nil, fmt.Errorf("greeting: %w", err)
	}
	if err := p.ok("USER " + user); err != nil {
		return nil, err
	}
	if err := p.ok("PASS " + pass); err != nil {
		return nil, err
	}
	defer p.ok("QUIT") //nolint:errcheck

	sizes, err := p.list()
	if err != nil {
		return nil, err
	}
	// Newest first: the probe was just delivered, and a maildrop with a history
	// would otherwise be read from its oldest end.
	for i := len(sizes) - 1; i >= 0; i-- {
		headers, err := p.top(sizes[i].num)
		if err != nil {
			return nil, err
		}
		subject := headerValue(headers, "Subject")
		if !strings.Contains(subject, marker) {
			continue
		}
		return newReading(surfPOP3).
			field("size", strconv.FormatInt(sizes[i].octets, 10)).
			field("subject", subject), nil
	}
	return nil, fmt.Errorf("the probe (marker %s) is not in the maildrop", marker)
}

type pop3Client struct {
	conn net.Conn
	r    *bufio.Reader
}

type pop3Entry struct {
	num    int
	octets int64
}

func (p *pop3Client) line() (string, error) {
	l, err := p.r.ReadString('\n')
	return strings.TrimRight(l, "\r\n"), err
}

func (p *pop3Client) ok(cmd string) error {
	fmt.Fprintf(p.conn, "%s\r\n", cmd)
	l, err := p.line()
	if err != nil {
		return fmt.Errorf("%s: %w", firstWord(cmd), err)
	}
	if !strings.HasPrefix(l, "+OK") {
		return fmt.Errorf("%s refused: %s", firstWord(cmd), l)
	}
	return nil
}

func (p *pop3Client) list() ([]pop3Entry, error) {
	if err := p.ok("LIST"); err != nil {
		return nil, err
	}
	var out []pop3Entry
	for {
		l, err := p.line()
		if err != nil {
			return nil, fmt.Errorf("LIST: %w", err)
		}
		if l == "." {
			return out, nil
		}
		var e pop3Entry
		if _, err := fmt.Sscanf(l, "%d %d", &e.num, &e.octets); err == nil {
			out = append(out, e)
		}
	}
}

// top reads a message's headers only: the row needs the subject to recognise
// the probe, and pulling whole bodies out of a maildrop to find one message is
// a cost the gate does not need to pay.
func (p *pop3Client) top(num int) ([]string, error) {
	if err := p.ok(fmt.Sprintf("TOP %d 0", num)); err != nil {
		return nil, err
	}
	var out []string
	for {
		l, err := p.line()
		if err != nil {
			return nil, fmt.Errorf("TOP: %w", err)
		}
		if l == "." {
			return out, nil
		}
		out = append(out, strings.TrimPrefix(l, "."))
	}
}

func headerValue(headers []string, name string) string {
	prefix := strings.ToLower(name) + ":"
	for i, h := range headers {
		if !strings.HasPrefix(strings.ToLower(h), prefix) {
			continue
		}
		value := strings.TrimSpace(h[len(prefix):])
		// Folded continuation lines belong to the same header.
		for j := i + 1; j < len(headers); j++ {
			if !strings.HasPrefix(headers[j], " ") && !strings.HasPrefix(headers[j], "\t") {
				break
			}
			value += strings.TrimSpace(headers[j])
		}
		return value
	}
	return ""
}

func firstWord(s string) string {
	if i := strings.IndexByte(s, ' '); i > 0 {
		return s[:i]
	}
	return s
}
