// Package ftsproto is the TAB-delimited, LF-terminated wire protocol between
// session processes and the yarilo-fts service, plus the client. One
// protocol for both modes (embedded and remote), the pkg/locks pattern.
//
//	> VERSION\t1\n                                  — handshake
//	< VERSION\t1\tOK\n
//	> INDEX\t<user>\t<folder>\t<guid>\t<uidvalidity>\t<maxuid>\t<maxrecent>\n
//	> PREPEND\t<user>\t<folder>\t<guid>\t<uidvalidity>\t<maxuid>\n
//	> EXPUNGE\t<user>\t<folder>\t<guid>\t<uidvalidity>\t<uid>\n
//	> LOOKUP\t<user>\t<folder>\t<guid>\t<uidvalidity>\t<query-b64json>\n
//	> STATUS\t<user>\t<folder>\t<guid>\t<uidvalidity>\n
//	> RESCAN\t<user>\t<folder>\t<guid>\t<uidvalidity>\n
//	> OPTIMIZE\t<user>\n
//	< OK[\t<payload>]\n | NO\t<message>\n | NO\t<code>\t<message>\n
//
// LOOKUP's payload is base64(JSON fts.Result); STATUS's payload is
// "<lastuid>\t<checksum>". Fields never contain TAB/LF (usernames and folder
// names are validated upstream); the query travels base64-encoded.
package ftsproto

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/pkg/fts"
)

const (
	ProtocolVersion = "1"

	CmdVersion  = "VERSION"
	CmdIndex    = "INDEX"
	CmdPrepend  = "PREPEND"
	CmdExpunge  = "EXPUNGE"
	CmdLookup   = "LOOKUP"
	CmdStatus   = "STATUS"
	CmdRescan   = "RESCAN"
	CmdOptimize = "OPTIMIZE"

	replyOK = "OK"
	replyNO = "NO"

	// CodeUnavailable rides inside a NO, in a field of its own, rather than as
	// a reply verb of its own.
	//
	// That choice is what makes a mixed rollout safe without ordering: a
	// reader that does not know the code sees a NO whose text happens to start
	// with a word, which is exactly what it did before. IMAP carries
	// [UNAVAILABLE] inside NO for the same reason, so this is the shape we
	// already answer clients with, one floor up (#1409).
	CodeUnavailable = "UNAVAILABLE"
)

// Service is the operation surface both the wire server and the embedded
// client dispatch into. Implemented by internal/ftsservice.
type Service interface {
	Index(user string, mbox fts.MailboxRef, maxUID uint32, maxRecent int) error
	Prepend(user string, mbox fts.MailboxRef, maxUID uint32) error
	Expunge(user string, mbox fts.MailboxRef, uid uint32) error
	Lookup(user string, mbox fts.MailboxRef, q fts.Query) (fts.Result, error)
	Status(user string, mbox fts.MailboxRef) (lastUID, checksum uint32, err error)
	Rescan(user string, mbox fts.MailboxRef) error
	Optimize(user string) error
}

// Client is what sessions program against: either Remote (wire) or the
// embedded Service directly.
type Client interface {
	Service
	Close() error
}

// Embedded wraps an in-process Service as a Client (tests / CLI runs).
type Embedded struct{ Service }

func (Embedded) Close() error { return nil }

// EncodeQuery / DecodeQuery carry an fts.Query as one TAB-safe field.
func EncodeQuery(q fts.Query) (string, error) {
	b, err := json.Marshal(q)
	if err != nil {
		return "", fmt.Errorf("ftsproto: encode query: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func DecodeQuery(s string) (fts.Query, error) {
	var q fts.Query
	b, err := base64.StdEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, &q)
	}
	if err != nil {
		return fts.Query{}, fmt.Errorf("ftsproto: decode query: %w", err)
	}
	return q, nil
}

// EncodeResult / DecodeResult carry an fts.Result the same way.
func EncodeResult(r fts.Result) (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("ftsproto: encode result: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func DecodeResult(s string) (fts.Result, error) {
	var r fts.Result
	b, err := base64.StdEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, &r)
	}
	if err != nil {
		return fts.Result{}, fmt.Errorf("ftsproto: decode result: %w", err)
	}
	return r, nil
}

// MboxFields flattens a MailboxRef into its wire fields.
func MboxFields(m fts.MailboxRef) []string {
	return []string{m.Name, m.GUID, strconv.FormatUint(uint64(m.UIDValidity), 10)}
}

// ParseMbox reads the three mailbox fields.
func ParseMbox(folder, guid, uidv string) (fts.MailboxRef, error) {
	v, err := strconv.ParseUint(uidv, 10, 32)
	if err != nil {
		return fts.MailboxRef{}, fmt.Errorf("ftsproto: bad uidvalidity %q", uidv)
	}
	return fts.MailboxRef{Name: folder, GUID: guid, UIDValidity: uint32(v)}, nil
}

// Remote is the wire client. Safe for sequential use; a mutex serialises
// request/response pairs on the single connection.
type Remote struct {
	mu   sync.Mutex
	conn net.Conn
	br   *bufio.Reader
}

// Dial connects and performs the VERSION handshake.
func Dial(addr string, timeout time.Duration) (*Remote, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("ftsproto: dial: %w", err)
	}
	r := &Remote{conn: conn, br: bufio.NewReader(conn)}
	line, err := r.roundTrip(CmdVersion + "\t" + ProtocolVersion)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if line != CmdVersion+"\t"+ProtocolVersion+"\tOK" {
		conn.Close()
		return nil, fmt.Errorf("ftsproto: handshake mismatch: %q", line)
	}
	return r, nil
}

func (r *Remote) Close() error { return r.conn.Close() }

func (r *Remote) roundTrip(req string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.conn.Write([]byte(req + "\n")); err != nil {
		return "", fmt.Errorf("ftsproto: write: %w", err)
	}
	line, err := r.br.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("ftsproto: read: %w", err)
	}
	return strings.TrimRight(line, "\n"), nil
}

// call sends a command and maps OK/NO to (payload, error).
func (r *Remote) call(fields ...string) (string, error) {
	line, err := r.roundTrip(strings.Join(fields, "\t"))
	if err != nil {
		return "", err
	}
	verb, rest, _ := strings.Cut(line, "\t")
	switch verb {
	case replyOK:
		return rest, nil
	case replyNO:
		return "", refusalError(rest)
	default:
		return "", fmt.Errorf("ftsproto: unexpected reply %q", line)
	}
}

// ErrUnavailable marks an FTS failure that was a dependency of the service
// being unreachable, not the service refusing. Recovered from the wire so the
// classification survives the process boundary: a sentinel wrapped inside
// yarilo-fts does not, which is why an outage used to reach clients as a bare
// NO (#1409).
var ErrUnavailable = errors.New("ftsproto: service unavailable")

// refusalError turns the text after NO into an error, recovering a code when
// one is present.
//
// Tolerant in both directions, deliberately. No code is the older shape and
// keeps its exact behaviour. An unknown code is TEXT, not a parse error: a
// reader that rejected codes it had not been taught would break on the next
// one added, which is the failure this form exists to avoid.
func refusalError(rest string) error {
	if code, text, ok := strings.Cut(rest, "\t"); ok && code == CodeUnavailable {
		return fmt.Errorf("ftsproto: server: %s: %w", text, ErrUnavailable)
	}
	return fmt.Errorf("ftsproto: server: %s", rest)
}

func (r *Remote) Index(user string, m fts.MailboxRef, maxUID uint32, maxRecent int) error {
	f := append([]string{CmdIndex, user}, MboxFields(m)...)
	f = append(f, strconv.FormatUint(uint64(maxUID), 10), strconv.Itoa(maxRecent))
	_, err := r.call(f...)
	return err
}

func (r *Remote) Prepend(user string, m fts.MailboxRef, maxUID uint32) error {
	f := append([]string{CmdPrepend, user}, MboxFields(m)...)
	f = append(f, strconv.FormatUint(uint64(maxUID), 10))
	_, err := r.call(f...)
	return err
}

func (r *Remote) Expunge(user string, m fts.MailboxRef, uid uint32) error {
	f := append([]string{CmdExpunge, user}, MboxFields(m)...)
	f = append(f, strconv.FormatUint(uint64(uid), 10))
	_, err := r.call(f...)
	return err
}

func (r *Remote) Lookup(user string, m fts.MailboxRef, q fts.Query) (fts.Result, error) {
	enc, err := EncodeQuery(q)
	if err != nil {
		return fts.Result{}, err
	}
	f := append([]string{CmdLookup, user}, MboxFields(m)...)
	f = append(f, enc)
	payload, err := r.call(f...)
	if err != nil {
		return fts.Result{}, err
	}
	return DecodeResult(payload)
}

func (r *Remote) Status(user string, m fts.MailboxRef) (uint32, uint32, error) {
	payload, err := r.call(append([]string{CmdStatus, user}, MboxFields(m)...)...)
	if err != nil {
		return 0, 0, err
	}
	last, sum, ok := strings.Cut(payload, "\t")
	if !ok {
		return 0, 0, fmt.Errorf("ftsproto: bad STATUS payload %q", payload)
	}
	l, err1 := strconv.ParseUint(last, 10, 32)
	s, err2 := strconv.ParseUint(sum, 10, 32)
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("ftsproto: bad STATUS payload %q", payload)
	}
	return uint32(l), uint32(s), nil
}

func (r *Remote) Rescan(user string, m fts.MailboxRef) error {
	_, err := r.call(append([]string{CmdRescan, user}, MboxFields(m)...)...)
	return err
}

func (r *Remote) Optimize(user string) error {
	_, err := r.call(CmdOptimize, user)
	return err
}
