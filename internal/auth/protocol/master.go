package protocol

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Master-protocol constants, versioned separately from the client protocol.
const (
	masterProtoName = "yarilo-auth-master"
	masterMajorVer  = 1
	masterMinorVer  = 0
)

// MasterServer answers password-less userdb lookups and user enumeration over a
// separate TCP+mTLS listener, using ID-tracked USER / PASS / LIST commands. Kept
// distinct from the client-protocol Server: no untrusted credentials cross this
// socket and it evolves on its own cadence.
type MasterServer struct {
	userdb           Userdb
	cache            *Cache
	tokenStore       TokenStore
	connUID          atomic.Uint64
	pid              int
	cookie           string
	defaultMailPath  string
	defaultInboxPath string
}

// MasterServerOption tunes a NewMasterServer construction.
type MasterServerOption func(*MasterServer)

// WithMasterCache attaches the auth cache that CACHE-FLUSH operates on. Pass the
// same *Cache the client-protocol Server got via WithCache so flushing here empties
// the cache serving login pods. Nil cache makes CACHE-FLUSH respond FAIL.
func WithMasterCache(c *Cache) MasterServerOption {
	return func(s *MasterServer) { s.cache = c }
}

// WithMasterTokenStore attaches the shared token store so SESSION can issue tokens
// that the client-protocol VERIFY validates (both servers need the same instance).
// Nil store makes SESSION respond FAIL.
func WithMasterTokenStore(ts TokenStore) MasterServerOption {
	return func(s *MasterServer) { s.tokenStore = ts }
}

// WithMasterDefaultMailPath sets the cluster-wide mail_path default
// applied when the userdb result carries no explicit mail_path.
func WithMasterDefaultMailPath(p string) MasterServerOption {
	return func(s *MasterServer) { s.defaultMailPath = p }
}

// WithMasterDefaultInboxPath sets the cluster-wide mail_inbox_path
// default applied when the userdb result carries no explicit value.
func WithMasterDefaultInboxPath(p string) MasterServerOption {
	return func(s *MasterServer) { s.defaultInboxPath = p }
}

func (s *MasterServer) applyMailPathDefaults(ui *UserInfo) {
	if ui.MailLocation != "" {
		ui.MailLocation = mailbox.ExpandVars(ui.MailLocation, ui.Username)
	}
	if ui.MailPath == "" && s.defaultMailPath != "" {
		mp := mailbox.ExpandHome(s.defaultMailPath, ui.Home)
		mp = strings.ReplaceAll(mp, "%h", ui.Home)
		ui.MailPath = mailbox.ExpandVars(mp, ui.Username)
	}
	if ui.InboxPath == "" && s.defaultInboxPath != "" {
		ip := mailbox.ExpandHome(s.defaultInboxPath, ui.Home)
		ip = strings.ReplaceAll(ip, "%h", ui.Home)
		ui.InboxPath = mailbox.ExpandVars(ip, ui.Username)
	}
}

// NewMasterServer constructs a MasterServer rooted at userdb (which may be a
// UserdbChain; LIST uses UserdbIterator when implemented). A nil userdb is legal:
// USER responds NOTFOUND and LIST responds FAIL, exposing only the wire surface.
func NewMasterServer(userdb Userdb, opts ...MasterServerOption) *MasterServer {
	cookie := make([]byte, 16)
	rand.Read(cookie) //nolint:errcheck
	s := &MasterServer{
		userdb: userdb,
		pid:    os.Getpid(),
		cookie: hex.EncodeToString(cookie),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Listen binds addr and returns the listener so a caller can report readiness
// only once the port accepts. Non-nil tlsCfg makes the listener use mTLS.
func (s *MasterServer) Listen(addr string, tlsCfg *tls.Config) (net.Listener, error) {
	if tlsCfg != nil {
		ln, err := tls.Listen("tcp", addr, tlsCfg)
		if err != nil {
			return nil, fmt.Errorf("auth/master: listen %s (tls): %w", addr, err)
		}
		return ln, nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("auth/master: listen %s: %w", addr, err)
	}
	return ln, nil
}

// ListenAndServe binds addr and serves it.
func (s *MasterServer) ListenAndServe(ctx context.Context, addr string, tlsCfg *tls.Config) error {
	ln, err := s.Listen(addr, tlsCfg)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve serves an already-bound listener.
func (s *MasterServer) Serve(ctx context.Context, ln net.Listener) error {

	var wg sync.WaitGroup
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			wg.Wait()
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("auth/master: accept: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleConn(conn)
		}()
	}
}

// handleConn drives the per-connection greet then serve loop. Commands on one
// connection run serially; concurrency comes from opening multiple connections.
func (s *MasterServer) handleConn(conn net.Conn) {
	defer conn.Close()
	noteMasterConn()
	cuid := s.connUID.Add(1)
	rd := bufio.NewReaderSize(conn, maxLine)

	fmt.Fprintf(conn, "VERSION\t%s\t%d\t%d\n", masterProtoName, masterMajorVer, masterMinorVer)
	fmt.Fprintf(conn, "SPID\t%d\n", s.pid)
	fmt.Fprintf(conn, "CUID\t%d\n", cuid)
	fmt.Fprintf(conn, "COOKIE\t%s\n", s.cookie)
	fmt.Fprintf(conn, "DONE\n")

	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				_ = err
			}
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		switch fields[0] {
		case "VERSION":
			// Client version handshake, ignored (any 1.x accepted).
		case "CPID":
			// Client pid notice, informational, no reply.
		case "USER":
			noteMasterRequest("USER")
			s.handleUser(conn, fields)
		case "SESSION":
			noteMasterRequest("SESSION")
			s.handleSession(conn, fields)
		case "PASS":
			// Declared in the wire surface; not yet implemented.
			noteMasterRequest("PASS")
			id := parseID(fields)
			fmt.Fprintf(conn, "FAIL\t%s\treason=PASS not implemented (Phase AUTH-2)\n", id)
		case "LIST":
			noteMasterRequest("LIST")
			s.handleList(conn, fields)
		case "CACHE-FLUSH":
			noteMasterRequest("CACHE-FLUSH")
			s.handleCacheFlush(conn, fields)
		default:
			// Counted under one constant label, never under the verb the
			// client sent: a label taken from the wire lets any caller grow
			// the metric's cardinality without limit.
			noteMasterRequest("unknown")
			id := parseID(fields)
			fmt.Fprintf(conn, "FAIL\t%s\treason=unknown command %q\n", id, fields[0])
		}
	}
}

// handleUser runs the userdb lookup for `USER <id> <username>
// [<key>=<value>...]`. Responds:
//
//	USER <id> <username>\t<fields>  on hit
//	NOTFOUND <id>                   on (nil, nil)
//	FAIL <id> reason=...            on backend error
func (s *MasterServer) handleUser(conn net.Conn, fields []string) {
	if len(fields) < 3 {
		id := parseID(fields)
		fmt.Fprintf(conn, "FAIL\t%s\treason=USER requires id + username\n", id)
		return
	}
	id, username := fields[1], fields[2]
	if s.userdb == nil {
		fmt.Fprintf(conn, "NOTFOUND\t%s\n", id)
		return
	}
	ui, err := s.userdb.Lookup(username)
	if err != nil {
		fmt.Fprintf(conn, "FAIL\t%s\treason=%s\n", id, escapeReason(err.Error()))
		return
	}
	if ui == nil {
		fmt.Fprintf(conn, "NOTFOUND\t%s\n", id)
		return
	}
	s.applyMailPathDefaults(ui)
	out := marshalUserInfo(ui)
	if out == "" {
		fmt.Fprintf(conn, "USER\t%s\t%s\n", id, username)
		return
	}
	fmt.Fprintf(conn, "USER\t%s\t%s\t%s\n", id, username, out)
}

// handleList streams one `LIST <id> <username>` line per enumerable user,
// terminated by `DONE <id>`. FAILs when the userdb is not a UserdbIterator.
func (s *MasterServer) handleList(conn net.Conn, fields []string) {
	id := parseID(fields)
	if s.userdb == nil {
		fmt.Fprintf(conn, "FAIL\t%s\treason=no userdb configured\n", id)
		return
	}
	iter, ok := s.userdb.(UserdbIterator)
	if !ok {
		fmt.Fprintf(conn, "FAIL\t%s\treason=userdb does not support enumeration\n", id)
		return
	}
	users, err := iter.Iterate()
	if err != nil {
		fmt.Fprintf(conn, "FAIL\t%s\treason=%s\n", id, escapeReason(err.Error()))
		return
	}
	for _, u := range users {
		fmt.Fprintf(conn, "LIST\t%s\t%s\n", id, u)
	}
	fmt.Fprintf(conn, "DONE\t%s\n", id)
}

// handleCacheFlush evicts cache entries matching the supplied user-masks:
//
//	CACHE-FLUSH <id>                  → full flush
//	CACHE-FLUSH <id> <mask> [<mask>…] → selective flush
//
// Responds `OK <id> <count>` with entries removed, or FAIL when no cache was
// configured via WithMasterCache.
func (s *MasterServer) handleCacheFlush(conn net.Conn, fields []string) {
	id := parseID(fields)
	if s.cache == nil {
		fmt.Fprintf(conn, "FAIL\t%s\treason=no cache configured\n", id)
		return
	}
	var n uint32
	if len(fields) <= 2 {
		n = s.cache.Clear()
	} else {
		n = s.cache.ClearByUserMask(fields[2:])
	}
	fmt.Fprintf(conn, "OK\t%s\t%d\n", id, n)
}

// handleSession issues a session token for username without passdb verification;
// the token is consumable only by VERIFY on the client listener:
//
//	SESSION <id> user=<username> sid=<warden-session-id> ip=<mta-ip>
//	→ OK <id> token=<tok>
//	→ FAIL <id> reason=...
func (s *MasterServer) handleSession(conn net.Conn, fields []string) {
	id := parseID(fields)
	if s.tokenStore == nil {
		fmt.Fprintf(conn, "FAIL\t%s\treason=token store not configured\n", id)
		return
	}
	var username, sid, ip string
	for _, f := range fields[2:] {
		switch {
		case strings.HasPrefix(f, "user="):
			username = strings.TrimPrefix(f, "user=")
		case strings.HasPrefix(f, "sid="):
			sid = strings.TrimPrefix(f, "sid=")
		case strings.HasPrefix(f, "ip="):
			ip = strings.TrimPrefix(f, "ip=")
		}
	}
	if username == "" {
		fmt.Fprintf(conn, "FAIL\t%s\treason=user required\n", id)
		return
	}
	tok, err := s.tokenStore.Issue(username, sid, "lmtp")
	if err != nil {
		fmt.Fprintf(conn, "FAIL\t%s\treason=%s\n", id, escapeReason(err.Error()))
		return
	}
	slog.Info("auth/master: session issued", "user", username, "sid", sid, "ip", ip)
	fmt.Fprintf(conn, "OK\t%s\ttoken=%s\n", id, tok)
}

// parseID extracts the request id from a command frame, returning "0" when the
// frame is malformed so a FAIL response still carries a correlatable id.
func parseID(fields []string) string {
	if len(fields) < 2 {
		return "0"
	}
	return fields[1]
}

// marshalUserInfo serialises a UserInfo into tab-separated key=value pairs.
// VisitFields already drops internal-only fields; this just joins the rest.
func marshalUserInfo(ui *UserInfo) string {
	if ui == nil {
		return ""
	}
	var parts []string
	ui.VisitFields(func(k, v string) {
		parts = append(parts, k+"="+escapeValue(v))
	})
	return strings.Join(parts, "\t")
}

// escapeValue keeps tab/newline/NUL from breaking the line-oriented framing:
// TAB→`\t`, LF→`\n`, NUL→`\0`, backslash→`\\`.
func escapeValue(v string) string {
	if !strings.ContainsAny(v, "\t\n\x00\\") {
		return v
	}
	var b strings.Builder
	b.Grow(len(v))
	for _, r := range v {
		switch r {
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case 0:
			b.WriteString(`\0`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escapeReason is escapeValue for the `reason=` field of FAIL responses.
func escapeReason(v string) string { return escapeValue(v) }
