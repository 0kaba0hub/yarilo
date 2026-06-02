package protocol

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Master-protocol constants. Versioned separately from the client
// protocol because admin tooling and backend-api speak through here
// and may evolve on a different cadence.
const (
	masterProtoName = "yarilo-auth-master"
	masterMajorVer  = 1
	masterMinorVer  = 0
)

// MasterServer answers password-less userdb lookups and user
// enumeration over a separate TCP+mTLS listener. Wire-format mirrors
// Dovecot's auth-master-connection at the shape level (USER / PASS /
// LIST commands with ID-tracked responses) so admin tooling that
// already knows the Dovecot dialect maps over without surprises.
//
// MasterServer is intentionally separate from the client-protocol
// Server: different audience (admins + backend-api vs login pods),
// different threat model (no untrusted credentials cross this
// socket), and different evolution cadence. The two share mTLS
// material today; splitting is a future operational knob.
type MasterServer struct {
	userdb  Userdb
	connUID atomic.Uint64
	pid     int
	cookie  string
}

// NewMasterServer constructs a MasterServer rooted at the given
// userdb. The userdb may be a UserdbChain composing multiple
// backends; LIST opportunistically uses UserdbIterator via type
// assertion when the supplied userdb implements it.
//
// Passing a nil userdb is legal — every USER lookup will respond
// NOTFOUND, LIST will respond FAIL. Useful for deployments that
// expose only the wire surface (e.g. for connectivity checks)
// without configured backends.
func NewMasterServer(userdb Userdb) *MasterServer {
	cookie := make([]byte, 16)
	rand.Read(cookie) //nolint:errcheck
	return &MasterServer{
		userdb: userdb,
		pid:    os.Getpid(),
		cookie: hex.EncodeToString(cookie),
	}
}

// ListenAndServe accepts connections until ctx is cancelled. When
// tlsCfg is non-nil the listener uses mTLS (TLS 1.3,
// RequireAndVerifyClientCert is the caller's responsibility — set
// it on tlsCfg). Each connection is served by its own goroutine;
// active conns drain before the function returns.
func (s *MasterServer) ListenAndServe(ctx context.Context, addr string, tlsCfg *tls.Config) error {
	var ln net.Listener
	var err error
	if tlsCfg != nil {
		ln, err = tls.Listen("tcp", addr, tlsCfg)
	} else {
		ln, err = net.Listen("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("auth/master: listen %s: %w", addr, err)
	}

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

// handleConn drives the per-connection greet → serve loop. Commands
// inside one connection are processed serially; client-side
// concurrency is achieved by opening multiple connections.
func (s *MasterServer) handleConn(conn net.Conn) {
	defer conn.Close()
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
			// Client version handshake — currently ignored (any
			// 1.x client is accepted). Reject when a 2.x major
			// version starts shipping.
		case "CPID":
			// Client pid notice — informational, no reply.
		case "USER":
			s.handleUser(conn, fields)
		case "PASS":
			// Phase AUTH-1 PR 2 declares PASS in the wire surface
			// so pkg/authclient can ship the full method set;
			// implementation lands with Passdb.LookupCredentials
			// in Phase AUTH-2.
			id := parseID(fields)
			fmt.Fprintf(conn, "FAIL\t%s\treason=PASS not implemented (Phase AUTH-2)\n", id)
		case "LIST":
			s.handleList(conn, fields)
		default:
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
	out := marshalUserInfo(ui)
	if out == "" {
		fmt.Fprintf(conn, "USER\t%s\t%s\n", id, username)
		return
	}
	fmt.Fprintf(conn, "USER\t%s\t%s\t%s\n", id, username, out)
}

// handleList streams every user the backend can enumerate, one
// `LIST <id> <username>` line per user, terminated by `DONE <id>`.
// FAILs when the configured userdb does not implement
// UserdbIterator (e.g. an LDAP backend with no enumerate filter).
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

// parseID extracts the request id from a command frame. Returns
// "0" when the frame is malformed; the response still carries an id
// so the client side can correlate the FAIL with the offending
// request even when the parse failed.
func parseID(fields []string) string {
	if len(fields) < 2 {
		return "0"
	}
	return fields[1]
}

// marshalUserInfo serialises a UserInfo into the tab-separated
// key=value wire form. Zero-valued fields are skipped (Dovecot
// auth-fields convention). Internal-only fields (Password,
// CertName, PolicyResponse) are stripped UNCONDITIONALLY so a
// password-less master-protocol consumer can never observe
// credentials even if a buggy Userdb backend populates them.
//
// Lists are comma-joined; booleans emit `key=yes` only when true;
// maps spread as one pair per entry (Forward keys gain the
// `forward_` prefix; Extra emits as-is).
func marshalUserInfo(ui *UserInfo) string {
	if ui == nil {
		return ""
	}
	var parts []string
	add := func(k, v string) {
		if v == "" {
			return
		}
		parts = append(parts, k+"="+escapeValue(v))
	}
	addInt := func(k string, v int) {
		if v == 0 {
			return
		}
		parts = append(parts, k+"="+strconv.Itoa(v))
	}
	addUint32 := func(k string, v uint32) {
		if v == 0 {
			return
		}
		parts = append(parts, k+"="+strconv.FormatUint(uint64(v), 10))
	}
	addBool := func(k string, v bool) {
		if !v {
			return
		}
		parts = append(parts, k+"=yes")
	}
	addList := func(k string, v []string) {
		if len(v) == 0 {
			return
		}
		parts = append(parts, k+"="+escapeValue(strings.Join(v, ",")))
	}

	add("original_user", ui.OriginalUser)
	add("master_user", ui.MasterUser)
	add("login_user", ui.LoginUser)

	addUint32("uid", ui.UID)
	addUint32("gid", ui.GID)
	add("home", ui.Home)
	add("chroot", ui.Chroot)
	add("system_groups_user", ui.SystemGroupsUser)
	addList("groups", ui.Groups)
	addBool("client_cert_present", ui.ClientCertPresent)

	add("mail", ui.MailLocation)
	addUint32("mail_uid", ui.MailUID)
	addUint32("mail_gid", ui.MailGID)
	add("mailbox_format", ui.MailboxFormat)
	add("mail_attribute_dict", ui.MailAttributeDict)

	addList("quota_rule", ui.QuotaRules)
	add("quota_over_flag", ui.QuotaOverFlag)

	addList("allow_nets", ui.AllowNets)

	addBool("nologin", ui.NoLogin)
	addBool("nodelay", ui.NoDelay)
	addBool("noauthenticate", ui.NoAuthenticate)
	addBool("pass_expired", ui.PassExpired)
	addBool("nopassword", ui.NoPassword)

	addBool("proxy", ui.Proxy)
	addBool("proxy_maybe", ui.ProxyMaybe)
	add("host", ui.Host)
	addInt("port", ui.Port)
	add("destuser", ui.DestUser)
	add("proxy_mech", ui.ProxyMech)
	addInt("proxy_timeout", ui.ProxyTimeout)
	addBool("proxy_redirect_reauth", ui.ProxyRedirectReauth)
	addBool("proxy_nopipelining", ui.ProxyNoPipelining)
	add("ssl", ui.SSL)
	addBool("starttls", ui.StartTLS)

	addInt("mail_max_userip_connections", ui.MailMaxUserIPConnections)
	addInt("mail_max_user_connections", ui.MailMaxUserConnections)

	add("service", ui.Service)
	add("local_name", ui.LocalName)

	// Forward + Extra get deterministic key order so two processes
	// serialising the same UserInfo produce byte-identical wire bytes.
	for _, k := range sortedKeys(ui.Forward) {
		parts = append(parts, "forward_"+k+"="+escapeValue(ui.Forward[k]))
	}
	for _, k := range sortedKeys(ui.Extra) {
		parts = append(parts, k+"="+escapeValue(ui.Extra[k]))
	}

	return strings.Join(parts, "\t")
}

// escapeValue stops tab / newline / NUL bytes in field values from
// breaking the line-oriented wire framing. Backslash escapes match
// Dovecot's convention (TAB→`\t`, LF→`\n`, NUL→`\0`, backslash→`\\`).
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

// escapeReason is escapeValue narrowed for the `reason=` field of
// FAIL responses; same escapes, separate name so callers reading
// the code know which context applies.
func escapeReason(v string) string { return escapeValue(v) }

// sortedKeys returns the keys of m in lexicographic order — the
// canonical key order the wire serialiser uses for map fields so
// two processes produce byte-identical bytes for the same UserInfo.
func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// inline insertion sort — len(m) is typically small (single-digit)
	// so the constant beats sort.Strings's overhead.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
