package pop3

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-sasl"

	"github.com/0kaba0hub/yarilo/internal/auth/oauth2"
	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	"github.com/0kaba0hub/yarilo/internal/auth/scram"
	"github.com/0kaba0hub/yarilo/internal/loginproto"
	"github.com/0kaba0hub/yarilo/pkg/locks"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

const (
	idleTimeout = 10 * time.Minute
	maxBadCmds  = 20
)

type pop3State int

const (
	stateAuth  pop3State = iota
	stateTrans           // authenticated, mailbox open
	stateDone
	statePreAuth // preamble received; mailbox setup pending
)

type session struct {
	srv                *Server
	conn               net.Conn
	br                 *bufio.Reader
	state              pop3State
	onTLS              bool
	remoteIP           net.IP   // real client IP; overridden by PreambleConn.RemoteAddr
	preAuthUser        string   // username from preamble; consumed in serve()
	preAuthHome        string   // userdb-resolved home from preamble
	preAuthMail        string   // userdb-resolved mail_location from preamble
	preAuthGroups      []string // userdb-resolved groups from preamble
	preAuthQuotaRules  []string // userdb-resolved quota rules from preamble
	preAuthVolatileDir string   // userdb-resolved volatile dir from preamble
	preAuthIndexDir    string   // userdb-resolved index dir from preamble
	preAuthControlDir  string   // userdb-resolved control dir from preamble
	preAuthAltDir      string   // userdb-resolved alt dir from preamble
	preAuthMailPath    string   // userdb-resolved mail root from preamble
	preAuthInboxPath   string   // userdb-resolved inbox path from preamble
	sid                string   // cross-service correlation ID from login-proxy

	// set after successful login
	lockKey         string
	sessionLockFile string // path to dotlock file; "" when not held
	limitIP         string // IP used for ConnLimit.Acquire; released in releaseLock
	pendingUser     string // temporary storage of USER arg before PASS arrives
	userInfo        *mailbox.UserInfo
	box             mailbox.UserMailbox
	idx             mailbox.UserIndex
	folder          *mailbox.Folder
	msgs            []*mailbox.MessageMeta
	deleted         []bool
	seenMsgs        []bool // tracks messages fetched via RETR this session
	uidls           []string
	lastMsg         int  // highest seq number RETR'd (RFC 1460 LAST)
	markedCorrupt   bool // INBOX already flagged FSCKD this session; gate repeat marks

	badCmds int
}

func (s *Server) newSession(conn net.Conn) *session {
	ip := net.IPv4zero
	if tcp, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		ip = tcp.IP
	}
	sess := &session{
		srv:      s,
		conn:     conn,
		br:       bufio.NewReaderSize(conn, 4096),
		state:    stateAuth,
		remoteIP: ip,
	}
	if pc, ok := conn.(*loginproto.PreambleConn); ok {
		sess.preAuthUser = pc.Username
		sess.preAuthHome = pc.Home
		sess.preAuthMail = pc.MailLoc
		sess.preAuthGroups = pc.Groups
		sess.preAuthQuotaRules = pc.QuotaRules
		sess.preAuthVolatileDir = pc.VolatileDir
		sess.preAuthIndexDir = pc.IndexDir
		sess.preAuthControlDir = pc.ControlDir
		sess.preAuthAltDir = pc.AltDir
		sess.preAuthMailPath = pc.MailPath
		sess.preAuthInboxPath = pc.InboxPath
		sess.sid = pc.SessionID
		sess.state = statePreAuth
	}
	return sess
}

func (s *session) serve() {
	defer s.conn.Close()
	defer s.releaseLock()

	s.setDeadline()
	s.ok("yarilo POP3 server ready")

	if s.state == statePreAuth {
		// Login pod has already authenticated the user and discards this
		// greeting. Set up the mailbox without sending an extra wire response.
		if !s.completePreAuth() {
			return
		}
	}

	for s.state != stateDone {
		line, err := s.readLine()
		if err != nil {
			break
		}
		s.setDeadline()
		s.dispatch(line)
		if s.badCmds >= maxBadCmds {
			s.writeErr("too many errors, closing")
			break
		}
	}
}

func (s *session) dispatch(line string) {
	cmd, arg, _ := strings.Cut(line, " ")
	cmd = strings.ToUpper(strings.TrimSpace(cmd))
	arg = strings.TrimSpace(arg)

	switch s.state {
	case stateAuth:
		s.handleAuth(cmd, arg)
	case stateTrans:
		s.handleTrans(cmd, arg)
	}
}

// ---- AUTH state ------------------------------------------------------------

func (s *session) handleAuth(cmd, arg string) {
	switch cmd {
	case "CAPA":
		s.sendCapa()
	case "USER":
		s.cmdUser(arg)
	case "PASS":
		s.cmdPass(arg)
	case "AUTH":
		s.cmdSASLAuth(arg)
	case "QUIT":
		s.ok("yarilo signing off")
		s.state = stateDone
	case "STLS":
		s.cmdSTLS()
	default:
		s.badCmd()
	}
}

func (s *session) cmdUser(arg string) {
	if arg == "" {
		s.writeErr("missing username")
		return
	}
	s.pendingUser = arg
	s.ok("send PASS")
}

func (s *session) cmdPass(arg string) {
	if s.pendingUser == "" {
		s.writeErr("USER required before PASS")
		return
	}
	username := s.pendingUser
	s.pendingUser = ""
	s.finishAuth("", username, arg)
}

// cmdSASLAuth implements POP3 SASL (RFC 5034): "AUTH <mech>
// [<base64-init>]". Supported mechanisms: PLAIN (always),
// OAUTHBEARER (when at least one OAuth provider is configured).
func (s *session) cmdSASLAuth(arg string) {
	parts := strings.SplitN(arg, " ", 2)
	if len(parts) == 0 || parts[0] == "" {
		s.writeErr("missing mechanism")
		return
	}
	mech := strings.ToUpper(parts[0])
	switch mech {
	case "PLAIN":
		s.handleSASLPlain(parts)
	case "OAUTHBEARER":
		if !s.srv.opts.OAuth2Enabled {
			s.writeErr("unsupported mechanism")
			return
		}
		s.handleSASLOAuthBearer(parts)
	case "XOAUTH2":
		if !s.srv.opts.OAuth2Enabled {
			s.writeErr("unsupported mechanism")
			return
		}
		s.handleSASLXOAuth2(parts)
	case "SCRAM-SHA-256":
		s.handleSASLScram(parts, false, s.scramSha256Builder())
	case "SCRAM-SHA-256-PLUS":
		s.handleSASLScram(parts, true, s.scramSha256Builder())
	case "SCRAM-SHA-1":
		s.handleSASLScram(parts, false, s.scramSha1Builder())
	case "SCRAM-SHA-1-PLUS":
		s.handleSASLScram(parts, true, s.scramSha1Builder())
	default:
		s.writeErr("unsupported mechanism")
	}
}

// scramBuilder captures the per-digest-family wiring so the
// shared handleSASLScram path does not need to know whether it is
// dispatching the SHA-256 or SHA-1 mech.
type scramBuilder struct {
	supported bool
	nonPlus   func(onSuccess func(string) error) *scram.Session
	plus      func(cb []byte, onSuccess func(string) error) *scram.Session
}

func (s *session) scramSha256Builder() scramBuilder {
	lookup, ok := s.srv.opts.Auth.(protocol.SCRAMSha256Lookup)
	if !ok {
		return scramBuilder{}
	}
	return scramBuilder{
		supported: true,
		nonPlus:   func(f func(string) error) *scram.Session { return scram.NewSha256(lookup, f) },
		plus:      func(cb []byte, f func(string) error) *scram.Session { return scram.NewSha256Plus(lookup, cb, f) },
	}
}

func (s *session) scramSha1Builder() scramBuilder {
	lookup, ok := s.srv.opts.Auth.(protocol.SCRAMSha1Lookup)
	if !ok {
		return scramBuilder{}
	}
	return scramBuilder{
		supported: true,
		nonPlus:   func(f func(string) error) *scram.Session { return scram.NewSha1(lookup, f) },
		plus:      func(cb []byte, f func(string) error) *scram.Session { return scram.NewSha1Plus(lookup, cb, f) },
	}
}

// handleSASLScram — AUTH SCRAM-SHA-{1,256} / SCRAM-SHA-{1,256}-PLUS
// (RFC 5802 / RFC 7677). Multi-round: client-first → server-
// first → client-final → server-final. Uses the shared SCRAM
// session adapter from internal/auth/scram so the success hook
// runs through the regular completeAuthenticated path. Digest
// family is wired in via the supplied scramBuilder.
func (s *session) handleSASLScram(parts []string, plus bool, b scramBuilder) {
	if !b.supported {
		s.writeErr("unsupported mechanism")
		return
	}
	var cb []byte
	if plus {
		cb = s.tlsExporter()
		if cb == nil {
			s.writeErr("channel binding unavailable")
			return
		}
	}

	// Capture the SCRAM-verified username via the OnSuccess hook
	// so the post-success path can run completeAuthenticated.
	var (
		verifiedUser string
		completed    bool
	)
	onSuccess := func(user string) error {
		verifiedUser = user
		completed = true
		return nil
	}
	var saslSrv *scram.Session
	if plus {
		saslSrv = b.plus(cb, onSuccess)
	} else {
		saslSrv = b.nonPlus(onSuccess)
	}

	if err := s.driveSASL(parts, saslSrv); err != nil {
		if d := s.srv.opts.FailureDelay; d > 0 {
			time.Sleep(d)
		}
		s.writeErr("authentication failed")
		return
	}
	if !completed {
		// driveSASL exited cleanly without the underlying server
		// reporting done=true with no err — defensive fall-through.
		s.writeErr("authentication failed")
		return
	}
	s.completeAuthenticated(&protocol.AuthResponse{
		Result:   protocol.AuthOK,
		Username: verifiedUser,
	})
}

// tlsExporter returns the 32-byte RFC 9266 channel-binding
// material derived from the underlying TLS conn. Returns nil
// for non-TLS or pre-TLS-1.3 connections.
func (s *session) tlsExporter() []byte {
	netConn := s.conn
	tc, ok := netConn.(*tls.Conn)
	if !ok {
		return nil
	}
	state := tc.ConnectionState()
	if state.Version < tls.VersionTLS13 {
		return nil
	}
	out, err := state.ExportKeyingMaterial("EXPORTER-Channel-Binding", nil, 32)
	if err != nil {
		return nil
	}
	return out
}

// driveSASL runs the multi-round SASL exchange. The initial
// response (when supplied via the AUTH line's second token) is
// fed to srv.Next first; otherwise an empty initial response is
// passed. For each non-final challenge the driver writes a
// `+ <base64>` continuation line and reads the next client
// line. Returns nil on done=true with no error; any error from
// the SASL server bubbles up.
func (s *session) driveSASL(parts []string, srv sasl.Server) error {
	var resp []byte
	// Initial response (optional in POP3 SASL).
	if len(parts) == 2 {
		decoded, err := base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return err
		}
		resp = decoded
	} else {
		fmt.Fprintf(s.conn, "+ \r\n")
	}
	for {
		challenge, done, err := srv.Next(resp)
		if err != nil {
			return err
		}
		if done {
			// On success the underlying SCRAM server returns the
			// final v=ServerSignature challenge. POP3 has no
			// way to deliver post-success SASL data, so we
			// surface the +OK without it — clients that depend
			// on ServerSignature verification (rare in POP3)
			// fall back to the no-server-sig codepath.
			_ = challenge
			fmt.Fprintf(s.conn, "+OK authenticated\r\n")
			return nil
		}
		// Continuation — emit + <base64-challenge>, read next.
		fmt.Fprintf(s.conn, "+ %s\r\n",
			base64.StdEncoding.EncodeToString(challenge))
		line, err := s.br.ReadString('\n')
		if err != nil {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "*" {
			return fmt.Errorf("authentication cancelled")
		}
		decoded, err := base64.StdEncoding.DecodeString(line)
		if err != nil {
			return err
		}
		resp = decoded
	}
}

// handleSASLPlain — the historical AUTH PLAIN path. Reads the
// initial response (or prompts for it), decodes, validates the
// NUL-separated authzid/authid/password tuple and dispatches via
// finishAuth.
func (s *session) handleSASLPlain(parts []string) {
	if s.srv.opts.DisablePlainAuth && !s.onTLS {
		s.writeErr("plaintext authentication disabled, use STLS first")
		return
	}
	payload, ok := s.readSASLPayload(parts)
	if !ok {
		return
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		s.writeErr("invalid base64")
		return
	}
	fields := strings.SplitN(string(decoded), "\x00", 3)
	if len(fields) != 3 {
		s.writeErr("invalid PLAIN response")
		return
	}
	// fields[0]=authzid, fields[1]=authid, fields[2]=password.
	s.finishAuth(fields[0], fields[1], fields[2])
}

// handleSASLOAuthBearer — AUTH OAUTHBEARER (RFC 7628). Reads the
// initial response, decodes, feeds it to a go-sasl OAUTHBEARER
// server which extracts the bearer token from the GS2 envelope
// and invokes our callback. Wire-shape concerns (GS2 parsing, JSON
// error blob on failure) live entirely in go-sasl.
func (s *session) handleSASLOAuthBearer(parts []string) {
	payload, ok := s.readSASLPayload(parts)
	if !ok {
		return
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		s.writeErr("invalid base64")
		return
	}
	srv := oauth2.NewOAuthBearerSASLServer(func(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
		s.finishAuth("", opts.Username, opts.Token)
		return nil
	})
	if _, _, err := srv.Next(decoded); err != nil {
		// go-sasl's OAUTHBEARER server returns errors only on
		// malformed input — never for backend rejection (those
		// surface via the callback's *OAuthBearerError, but
		// finishAuth already wrote the wire reply).
		if d := s.srv.opts.FailureDelay; d > 0 {
			time.Sleep(d)
		}
		s.writeErr("invalid OAUTHBEARER response")
		return
	}
	// finishAuth wrote +OK/-ERR; nothing else for us to do.
}

// handleSASLXOAuth2 mirrors handleSASLOAuthBearer for the XOAUTH2
// wire format (user=X\x01auth=Bearer T\x01\x01, no GS2 envelope).
func (s *session) handleSASLXOAuth2(parts []string) {
	payload, ok := s.readSASLPayload(parts)
	if !ok {
		return
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		s.writeErr("invalid base64")
		return
	}
	srv := oauth2.NewXOAuth2SASLServer(func(opts sasl.XOAuth2Options) *sasl.OAuthBearerError {
		s.finishAuth("", opts.Username, opts.Token)
		return nil
	})
	if _, _, err := srv.Next(decoded); err != nil {
		if d := s.srv.opts.FailureDelay; d > 0 {
			time.Sleep(d)
		}
		s.writeErr("invalid XOAUTH2 response")
		return
	}
}

// readSASLPayload returns the base64 SASL initial response. When
// the AUTH line carries no second token, prompts the client with
// "+\r\n" and reads the response line. Returns ok=false after
// writing the appropriate error reply.
func (s *session) readSASLPayload(parts []string) (string, bool) {
	if len(parts) == 2 {
		return parts[1], true
	}
	fmt.Fprintf(s.conn, "+ \r\n")
	line, err := s.br.ReadString('\n')
	if err != nil {
		return "", false
	}
	payload := strings.TrimRight(line, "\r\n")
	if payload == "*" {
		s.writeErr("authentication cancelled")
		return "", false
	}
	return payload, true
}

// finishAuth authenticates, resolves UserInfo, opens storage handles, and
// loads the mailbox. Used by both PASS (after USER) and AUTH PLAIN.
// authzid carries the master-user impersonation target (RFC 4616);
// USER/PASS path passes "" since the legacy command has no authzid
// surface.
func (s *session) finishAuth(authzid, username, password string) {
	if s.srv.opts.DisablePlainAuth && !s.onTLS {
		s.writeErr("plaintext authentication disabled, use STLS first")
		return
	}
	res, err := s.authenticate(authzid, username, password)
	if err != nil || res == nil || res.Result != protocol.AuthOK {
		// Timing-leak mitigation: hold the -ERR reply for the
		// configured delay so unknown-user / wrong-password
		// surface in the same wall-clock time.
		if d := s.srv.opts.FailureDelay; d > 0 {
			time.Sleep(d)
		}
		slog.Info("pop3: auth failed", "sid", s.sid, "user", username, "remoteIP", s.remoteIP, "result", "fail")
		s.writeErr("authentication failed")
		return
	}
	s.completeAuthenticated(res)
}

// completeAuthenticated runs the post-verify session setup
// (resolve userInfo, acquire connection limit + mailbox lock,
// open backends, load mailbox, write +OK). Shared between the
// password-verifying finishAuth path and the SCRAM SASL path
// where the credential is already verified by the mechanism.
func (s *session) completeAuthenticated(res *protocol.AuthResponse) {
	if !s.setupSession(res) {
		return
	}
	s.state = stateTrans
	s.ok(fmt.Sprintf("logged in, %d messages", len(s.msgs)))
}

// completePreAuth sets up the session for a login-pod pre-authenticated
// connection. Identical to setupSession but sends no wire response — the
// login pod has already told the client "+OK Logged in" and will discard
// the backend greeting. Returns false when setup fails (caller closes conn).
func (s *session) completePreAuth() bool {
	res := &protocol.AuthResponse{
		Result:      protocol.AuthOK,
		Username:    s.preAuthUser,
		Home:        s.preAuthHome,
		MailLoc:     s.preAuthMail,
		Groups:      s.preAuthGroups,
		QuotaRules:  s.preAuthQuotaRules,
		VolatileDir: s.preAuthVolatileDir,
		IndexDir:    s.preAuthIndexDir,
		ControlDir:  s.preAuthControlDir,
		AltDir:      s.preAuthAltDir,
		MailPath:    s.preAuthMailPath,
		InboxPath:   s.preAuthInboxPath,
	}
	ok := s.setupSession(res)
	if ok {
		s.state = stateTrans
	}
	return ok
}

// setupSession resolves UserInfo, acquires limits/locks, opens storage handles,
// and loads the mailbox. On failure it writes an error to the wire (for the
// normal auth path) and returns false. For preAuth callers, the error line is
// never seen by the client — the connection just closes.
func (s *session) setupSession(res *protocol.AuthResponse) bool {
	resolver := s.srv.opts.Resolver
	if resolver == nil {
		resolver = &mailbox.Resolver{}
	}
	userInfo := resolver.UserInfo(res.Username, res.Home)
	userInfo.Groups = res.Groups
	userInfo.QuotaRules = res.QuotaRules
	userInfo.SessionID = s.sid
	if res.VolatileDir != "" {
		vd := mailbox.ExpandHome(res.VolatileDir, userInfo.Home)
		vd = strings.ReplaceAll(vd, "%h", userInfo.Home)
		userInfo.VolatileDir = mailbox.ExpandVars(vd, res.Username)
	}
	if res.IndexDir != "" {
		id := mailbox.ExpandHome(res.IndexDir, userInfo.Home)
		id = strings.ReplaceAll(id, "%h", userInfo.Home)
		userInfo.IndexDir = mailbox.ExpandVars(id, res.Username)
	}
	if res.ControlDir != "" {
		cd := mailbox.ExpandHome(res.ControlDir, userInfo.Home)
		cd = strings.ReplaceAll(cd, "%h", userInfo.Home)
		userInfo.ControlDir = mailbox.ExpandVars(cd, res.Username)
	}
	if res.AltDir != "" {
		ad := mailbox.ExpandHome(res.AltDir, userInfo.Home)
		ad = strings.ReplaceAll(ad, "%h", userInfo.Home)
		userInfo.AltDir = mailbox.ExpandVars(ad, res.Username)
	}
	if res.MailPath != "" {
		mp := mailbox.ExpandHome(res.MailPath, userInfo.Home)
		mp = strings.ReplaceAll(mp, "%h", userInfo.Home)
		userInfo.MailPath = mailbox.ExpandVars(mp, res.Username)
	}
	if res.InboxPath != "" {
		ip := mailbox.ExpandHome(res.InboxPath, userInfo.Home)
		ip = strings.ReplaceAll(ip, "%h", userInfo.Home)
		userInfo.InboxPath = mailbox.ExpandVars(ip, res.Username)
	}

	if lim := s.srv.opts.ConnLimit; lim != nil {
		ip := s.remoteIP.String()
		if !lim.Acquire(userInfo.Username, ip) {
			slog.Warn("pop3: connection limit reached", "sid", s.sid, "user", userInfo.Username, "ip", ip, "result", "fail")
			s.writeErr("too many simultaneous connections")
			return false
		}
		s.limitIP = ip
	}

	if !s.srv.tryLock(userInfo.Username) {
		if s.srv.opts.ConnLimit != nil {
			s.srv.opts.ConnLimit.Release(userInfo.Username, s.limitIP)
			s.limitIP = ""
		}
		s.writeErr("mailbox already in use, try again later")
		return false
	}
	s.lockKey = userInfo.Username

	// Honour the per-user mail_location driver (sdbox/mdbox/maildir) exactly as
	// IMAP does: record it on userInfo so the fileindex picks the matching
	// per-folder layout, and select the per-user mailbox backend when the driver
	// differs from the global default. Without this POP3 opens every user
	// through the global (maildir) backend and reports 0 messages for dbox users.
	if err := mailbox.StampLocation(userInfo, res.MailLoc); err != nil {
		slog.Warn("pop3: mail_location parse failed; using global mailbox backend",
			"user", userInfo.Username, "mail_location", res.MailLoc, "err", err)
	}
	personalBox := mailbox.SelectPersonalBackend(s.srv.opts.Mailbox, s.srv.opts.MailboxByDriver, userInfo.Driver)
	box := personalBox.OpenUser(userInfo)
	idx := s.srv.opts.Index.OpenUser(userInfo)

	if err := box.Init(); err != nil {
		slog.Error("pop3: mailbox init", "user", userInfo.Username, "err", err)
		s.srv.unlock(s.lockKey)
		s.lockKey = ""
		if s.srv.opts.ConnLimit != nil {
			s.srv.opts.ConnLimit.Release(userInfo.Username, s.limitIP)
			s.limitIP = ""
		}
		s.writeErr("internal error")
		return false
	}

	// Dotlock is acquired after Init so the home directory exists on disk.
	if s.srv.opts.LockSession && !s.acquireDotlock(userInfo.Home) {
		s.srv.unlock(s.lockKey)
		s.lockKey = ""
		if s.srv.opts.ConnLimit != nil {
			s.srv.opts.ConnLimit.Release(userInfo.Username, s.limitIP)
			s.limitIP = ""
		}
		s.writeErr("mailbox already in use, try again later")
		return false
	}

	s.userInfo = userInfo
	s.box = box
	s.idx = idx

	if err := s.loadMailbox(); err != nil {
		s.writeErr("internal error")
		s.srv.unlock(s.lockKey)
		s.lockKey = ""
		return false
	}
	master, _ := res.Fields.Get("master_user")
	slog.Info("pop3: login",
		"sid", s.sid,
		"user", userInfo.Username,
		"master_user", master,
		"remoteIP", s.remoteIP,
		"messages", len(s.msgs),
		"result", "ok",
	)
	return true
}

// authenticate dispatches to MasterAuthenticator when authzid is
// set and the backend supports it; otherwise falls back to the
// regular Authenticator surface. Reject distinct authzid against a
// non-master backend with an opaque AuthFail so the wire reply
// stays indistinguishable from a wrong-password rejection.
func (s *session) authenticate(authzid, username, password string) (*protocol.AuthResponse, error) {
	ip := s.remoteIP.String()
	if authzid == "" || authzid == username {
		return s.srv.opts.Auth.Authenticate(username, password, "pop3", ip)
	}
	master, ok := s.srv.opts.Auth.(protocol.MasterAuthenticator)
	if !ok {
		return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
	}
	return master.AuthenticateMaster(authzid, username, password, "pop3", ip)
}

func (s *session) loadMailbox() error {
	folder, err := s.idx.OpenFolder("INBOX", uint32(time.Now().Unix()))
	if err != nil {
		slog.Error("pop3: open folder", "user", s.userInfo.Username, "err", err)
		return err
	}
	// Reactive self-heal: a dbox folder flagged corrupt (by an earlier read on
	// any protocol) is healed on POP3 login too, so a POP3-only mailbox does not
	// stay broken waiting for an IMAP SELECT that never comes.
	if folder.Fsckd {
		if rb, ok := s.box.(mailbox.ReactiveHealer); ok {
			// POP3 has no FTS client wired in, so the expunged UIDs cannot be
			// invalidated here — a POP3-only heal leaves FTS ghost documents until
			// the next rescan. The IMAP heal path and the operator rebuild both
			// notify FTS directly.
			//
			// No retry-bound here either (unlike the IMAP path's maxHealAttempts):
			// a POP3 session heals at most once, at login, not in a SELECT/STATUS/
			// IDLE loop, so one session cannot spin on incomplete scans. A client
			// that reconnects rapidly during a near-continuous purge could still
			// reproduce the scan storm across logins, but POP3 sessions are short
			// and a cross-login bound would need persistent state — an accepted gap.
			if expunged, herr := rb.HealCorruptFolder(s.idx, folder); herr != nil {
				slog.Warn("pop3: dbox reactive heal failed", "user", s.userInfo.Username, "err", herr)
			} else if len(expunged) > 0 {
				slog.Info("pop3: dbox reactive heal", "user", s.userInfo.Username, "expunged", len(expunged))
				if refreshed, rerr := s.idx.OpenFolder("INBOX", 0); rerr == nil {
					folder = refreshed
				}
			}
		}
	}
	msgs, err := s.idx.GetMessages(folder.ID, mailbox.SeqSet{})
	if err != nil {
		slog.Error("pop3: get messages", "user", s.userInfo.Username, "err", err)
		return err
	}
	var savedUIDLs map[uint32]string
	if s.srv.opts.SaveUIDL {
		if saved, err := s.idx.GetPOP3UIDLs(folder.ID); err != nil {
			slog.Warn("pop3: load saved uidls", "user", s.userInfo.Username, "err", err)
		} else {
			savedUIDLs = saved
		}
	}
	s.folder = folder
	s.msgs = msgs
	s.deleted = make([]bool, len(msgs))
	s.seenMsgs = make([]bool, len(msgs))
	s.computeUIDLs(savedUIDLs)
	return nil
}

// computeUIDLs pre-builds the UIDL string for every message.
// saved is a uid→uidl map from a prior session (nil or empty = no prior data).
// Priority: ReuseXUIDL header > saved index entry > format-computed value.
func (s *session) computeUIDLs(saved map[uint32]string) {
	s.uidls = make([]string, len(s.msgs))
	rename := s.srv.opts.UIDLDuplicates == "rename"
	seen := make(map[string]int)
	for i, m := range s.msgs {
		u := s.formatUIDL(m)
		if s.srv.opts.ReuseXUIDL {
			if xu := s.readXUIDL(m); xu != "" {
				u = xu
			}
		} else if v, ok := saved[m.UID]; ok && v != "" {
			u = v
		}
		if rename {
			base := u
			n := seen[base]
			seen[base]++
			if n > 0 {
				u = fmt.Sprintf("%s-%d", base, n+1)
			}
		}
		s.uidls[i] = u
	}
}

// readXUIDL reads the X-UIDL header from the raw message file.
func (s *session) readXUIDL(m *mailbox.MessageMeta) string {
	rc, err := s.fetchINBOX(m)
	if err != nil {
		return ""
	}
	defer rc.Close()
	hdr, err := textproto.NewReader(bufio.NewReader(rc)).ReadMIMEHeader()
	if err != nil && len(hdr) == 0 {
		return ""
	}
	return hdr.Get("X-Uidl")
}

// formatUIDL formats a UIDL string from opts.UIDLFormat.
func (s *session) formatUIDL(m *mailbox.MessageMeta) string {
	format := s.srv.opts.UIDLFormat
	if format == "" {
		format = "%u.%v"
	}
	var b strings.Builder
	i := 0
	for i < len(format) {
		if format[i] != '%' || i+1 >= len(format) {
			b.WriteByte(format[i])
			i++
			continue
		}
		i++ // skip '%'
		mod := ""
		for i < len(format) && !isUIDLVar(format[i]) {
			mod += string(format[i])
			i++
		}
		if i >= len(format) {
			b.WriteString("%" + mod)
			break
		}
		v := format[i]
		i++
		switch v {
		case 'u':
			b.WriteString(applyNumFmt(mod, uint64(m.UID)))
		case 'v':
			b.WriteString(applyNumFmt(mod, uint64(s.folder.UIDValidity)))
		case 'f':
			b.WriteString(m.Filename)
		case 'g':
			b.WriteString(hex.EncodeToString(m.GUID[:]))
		case 'm':
			h := md5.Sum([]byte(m.Filename))
			b.WriteString(hex.EncodeToString(h[:]))
		default:
			b.WriteByte('%')
			b.WriteString(mod)
			b.WriteByte(v)
		}
	}
	return b.String()
}

func isUIDLVar(c byte) bool {
	return c == 'u' || c == 'v' || c == 'f' || c == 'g' || c == 'm'
}

func applyNumFmt(mod string, val uint64) string {
	if mod == "" || mod == "d" {
		return strconv.FormatUint(val, 10)
	}
	return fmt.Sprintf("%"+mod, val)
}

func msgVSize(m *mailbox.MessageMeta) uint32 {
	if m.VSize > 0 {
		return m.VSize
	}
	return m.Size
}

func appendFlag(flags []string, flag string) []string {
	for _, f := range flags {
		if strings.EqualFold(f, flag) {
			return flags
		}
	}
	out := make([]string, len(flags)+1)
	copy(out, flags)
	out[len(flags)] = flag
	return out
}

func removeFlag(flags []string, flag string) []string {
	out := make([]string, 0, len(flags))
	for _, f := range flags {
		if !strings.EqualFold(f, flag) {
			out = append(out, f)
		}
	}
	return out
}

func (s *session) cmdSTLS() {
	if s.onTLS {
		s.writeErr("already on TLS")
		return
	}
	if s.srv.opts.TLSConfig == nil {
		s.writeErr("STLS not available")
		return
	}
	s.ok("Begin TLS negotiation")
	tlsConn := tls.Server(s.conn, s.srv.opts.TLSConfig)
	if err := tlsConn.Handshake(); err != nil {
		slog.Info("pop3: TLS handshake failed", "err", err)
		s.state = stateDone
		return
	}
	s.conn = tlsConn
	s.br = bufio.NewReader(tlsConn)
	s.onTLS = true
	s.pendingUser = "" // RFC 2595 §4: reset state after TLS upgrade
}

// ---- TRANSACTION state -----------------------------------------------------

func (s *session) handleTrans(cmd, arg string) {
	switch cmd {
	case "CAPA":
		s.sendCapa()
	case "STAT":
		s.cmdStat()
	case "LIST":
		s.cmdList(arg)
	case "RETR":
		s.cmdRetr(arg)
	case "DELE":
		s.cmdDele(arg)
	case "NOOP":
		s.ok("done")
	case "RSET":
		s.cmdRset()
	case "TOP":
		s.cmdTop(arg)
	case "UIDL":
		s.cmdUidl(arg)
	case "LAST":
		s.cmdLast()
	case "QUIT":
		s.cmdQuit()
	default:
		s.badCmd()
	}
}

func (s *session) cmdStat() {
	count, total := s.countActive()
	s.ok(fmt.Sprintf("%d %d", count, total))
}

func (s *session) cmdList(arg string) {
	if arg != "" {
		idx, ok := s.parseMsgNum(arg)
		if !ok {
			return
		}
		s.ok(fmt.Sprintf("%d %d", idx+1, msgVSize(s.msgs[idx])))
		return
	}
	count, total := s.countActive()
	s.ok(fmt.Sprintf("%d messages (%d octets)", count, total))
	for i, m := range s.msgs {
		if !s.deleted[i] {
			fmt.Fprintf(s.conn, "%d %d\r\n", i+1, msgVSize(m))
		}
	}
	s.writeDot()
}

// fetchINBOX reads a message body and flags the folder for a reactive heal if
// the read tripped over corrupt sdbox storage (missing/truncated/bad file).
func (s *session) fetchINBOX(m *mailbox.MessageMeta) (io.ReadCloser, error) {
	rc, err := s.box.Fetch("INBOX", m.Filename, m.AltTier)
	// Flag once per session: a RETR loop over a corrupt mailbox must not pay an
	// OpenFolder+mark per message — the single mark heals every missing record on
	// the next open. POP3 has no mid-session re-check (unlike IMAP's SELECT path):
	// if another session heals the folder after this mark, a later corruption in
	// this same session is not re-flagged until QUIT. Accepted — POP3 sessions are
	// short and the next login heals anyway.
	if err != nil && !s.markedCorrupt && mailbox.MarkCorruptOnFetchErr(s.box, s.idx, "INBOX", err) {
		s.markedCorrupt = true
	}
	return rc, err
}

func (s *session) cmdRetr(arg string) {
	idx, ok := s.parseMsgNum(arg)
	if !ok {
		return
	}
	m := s.msgs[idx]
	rc, err := s.fetchINBOX(m)
	if err != nil {
		slog.Error("pop3: fetch", "uid", m.UID, "err", err)
		s.writeErr("unable to fetch message")
		return
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		s.writeErr("read error")
		return
	}
	s.seenMsgs[idx] = true
	if idx+1 > s.lastMsg {
		s.lastMsg = idx + 1
	}
	s.ok(fmt.Sprintf("%d octets", len(data)))
	writeMultiLine(s.conn, data)
}

func (s *session) cmdDele(arg string) {
	idx, ok := s.parseMsgNum(arg)
	if !ok {
		return
	}
	s.deleted[idx] = true
	s.ok(fmt.Sprintf("message %d deleted", idx+1))
}

func (s *session) cmdRset() {
	tRset := time.Now()
	if s.srv.opts.EnableLast {
		for i, seen := range s.seenMsgs {
			if !seen {
				continue
			}
			m := s.msgs[i]
			newFlags := removeFlag(m.Flags, `\Seen`)
			if err := s.idx.UpdateFlags(s.folder.ID, m.UID, newFlags, m.Keywords); err != nil {
				slog.Error("pop3: rset remove seen", "uid", m.UID, "err", err)
			} else {
				m.Flags = newFlags
			}
			s.seenMsgs[i] = false
		}
		s.lastMsg = 0
	}
	for i := range s.deleted {
		s.deleted[i] = false
	}
	count, _ := s.countActive()
	slog.Debug("pop3: rset timing", "total_ms", time.Since(tRset).Milliseconds())
	s.ok(fmt.Sprintf("maildrop has %d messages", count))
}

func (s *session) cmdTop(arg string) {
	parts := strings.Fields(arg)
	if len(parts) != 2 {
		s.writeErr("usage: TOP <msg> <lines>")
		return
	}
	idx, ok := s.parseMsgNum(parts[0])
	if !ok {
		return
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil || n < 0 {
		s.writeErr("invalid line count")
		return
	}
	m := s.msgs[idx]
	rc, err := s.fetchINBOX(m)
	if err != nil {
		s.writeErr("unable to fetch message")
		return
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	s.ok("")
	writeTopLines(s.conn, data, n)
}

func (s *session) cmdUidl(arg string) {
	if arg != "" {
		idx, ok := s.parseMsgNum(arg)
		if !ok {
			return
		}
		s.ok(fmt.Sprintf("%d %s", idx+1, s.uidls[idx]))
		return
	}
	s.ok("")
	for i := range s.msgs {
		if !s.deleted[i] {
			fmt.Fprintf(s.conn, "%d %s\r\n", i+1, s.uidls[i])
		}
	}
	s.writeDot()
}

func (s *session) cmdLast() {
	if !s.srv.opts.EnableLast {
		s.badCmd()
		return
	}
	s.ok(fmt.Sprintf("%d", s.lastMsg))
}

// cmdQuit applies \Seen flags (unless NoFlagUpdates) and commits deletions.
func (s *session) cmdQuit() {
	tQuit := time.Now()
	var seenCount, deletedCount int
	if !s.srv.opts.NoFlagUpdates {
		for i, seen := range s.seenMsgs {
			if seen && !s.deleted[i] {
				m := s.msgs[i]
				newFlags := appendFlag(m.Flags, `\Seen`)
				if err := s.idx.UpdateFlags(s.folder.ID, m.UID, newFlags, m.Keywords); err != nil {
					slog.Error("pop3: set seen", "uid", m.UID, "err", err)
				} else {
					seenCount++
				}
			}
		}
	}

	if s.srv.opts.SaveUIDL {
		uidlMap := make(map[uint32]string, len(s.msgs))
		for i, m := range s.msgs {
			if !s.deleted[i] {
				uidlMap[m.UID] = s.uidls[i]
			}
		}
		if err := s.idx.SavePOP3UIDLs(s.folder.ID, uidlMap); err != nil {
			slog.Warn("pop3: save uidls", "user", s.userInfo.Username, "err", err)
		}
	}

	var errCount int
	if s.srv.opts.DeleteType == "flag" {
		deletedFlag := s.srv.opts.DeletedFlag
		if deletedFlag == "" {
			deletedFlag = "$POP3Deleted"
		}
		for i, del := range s.deleted {
			if !del {
				continue
			}
			m := s.msgs[i]
			newFlags := appendFlag(m.Flags, deletedFlag)
			if err := s.idx.UpdateFlags(s.folder.ID, m.UID, newFlags, m.Keywords); err != nil {
				slog.Error("pop3: flag deleted", "uid", m.UID, "err", err)
				errCount++
			} else {
				deletedCount++
			}
		}
	} else {
		for _, del := range s.deleted {
			if del {
				deletedCount++
			}
		}
		errCount = s.expungeDeleted()
	}

	// Release dotlock and in-memory lock before sending +OK so the next session
	// can acquire the lock as soon as it reads our response (not later when the
	// goroutine unwinds its defers).
	s.releaseLock()

	slog.Debug("pop3: quit timing",
		"user", s.userInfo.Username,
		"seen_updates", seenCount, "deleted", deletedCount,
		"total_ms", time.Since(tQuit).Milliseconds())

	if errCount > 0 {
		s.writeErr(fmt.Sprintf("%d message(s) could not be deleted", errCount))
	} else {
		s.ok("yarilo signing off")
	}
	s.state = stateDone
}

func (s *session) expungeDeleted() int {
	if s.srv.opts.Locker != nil && s.userInfo != nil {
		var errCount int
		key := locks.MailboxKey(s.userInfo.Username, "INBOX")
		owner := fmt.Sprintf("yarilo-pop3/%d/%s", os.Getpid(), s.userInfo.Username)
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		lk, err := locks.Acquire(ctx, s.srv.opts.Locker, key, owner, 30*time.Second)
		if err != nil {
			slog.Error("pop3: outer lock failed; falling back to per-message", "err", err)
			return s.expungeDeletedPerMessage()
		}
		defer func() { _ = s.srv.opts.Locker.Unlock(ctx, lk.ID) }()
		// Inside this scope the storage backends' withMailboxLock will see
		// HoldsResource and skip re-acquiring — the whole batch runs under
		// one X lock from cross-process observers' POV.
		for i, m := range s.msgs {
			if !s.deleted[i] {
				continue
			}
			if rerr := s.box.Remove("INBOX", m.Filename); rerr != nil {
				slog.Error("pop3: remove", "uid", m.UID, "err", rerr)
				errCount++
				continue
			}
			s.idx.ExpungeMessage(s.folder.ID, m.UID) //nolint:errcheck
			// Best-effort EXPUNGED EVENT so IMAP IDLE on sibling pods wakes
			// up. Reuses the same Locker connection that holds the outer
			// lock — Emit is independent of the active Lock.
			_ = s.srv.opts.Locker.Emit(ctx, key, locks.EventExpunged, strconv.FormatUint(uint64(m.UID), 10))
		}
		return errCount
	}
	return s.expungeDeletedPerMessage()
}

// expungeDeletedPerMessage is the legacy path used when no Locker is wired
// (single-process dev, tests). Each storage call takes its own X lock — N+M
// lock cycles per QUIT but per-message atomicity is preserved.
func (s *session) expungeDeletedPerMessage() int {
	var errCount int
	for i, m := range s.msgs {
		if !s.deleted[i] {
			continue
		}
		if err := s.box.Remove("INBOX", m.Filename); err != nil {
			slog.Error("pop3: remove", "uid", m.UID, "err", err)
			errCount++
		} else {
			s.idx.ExpungeMessage(s.folder.ID, m.UID) //nolint:errcheck
		}
	}
	return errCount
}

// ---- helpers ---------------------------------------------------------------

func (s *session) sendCapa() {
	s.ok("capability list follows")
	fmt.Fprintf(s.conn, "TOP\r\nUIDL\r\nUSER\r\nRESP-CODES\r\nPIPELINING\r\nAUTH-RESP-CODE\r\nSASL PLAIN\r\n")
	if s.srv.opts.EnableLast {
		fmt.Fprintf(s.conn, "LAST\r\n")
	}
	if s.srv.opts.TLSConfig != nil && !s.onTLS {
		fmt.Fprintf(s.conn, "STLS\r\n")
	}
	s.writeDot()
}

func (s *session) countActive() (count int, total int64) {
	for i, m := range s.msgs {
		if !s.deleted[i] {
			count++
			total += int64(msgVSize(m))
		}
	}
	return count, total
}

func (s *session) parseMsgNum(arg string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil || n < 1 || n > len(s.msgs) {
		s.writeErr(fmt.Sprintf("no such message, only %d messages in maildrop", len(s.msgs)))
		return 0, false
	}
	idx := n - 1
	if s.deleted[idx] {
		s.writeErr("message deleted")
		return 0, false
	}
	return idx, true
}

func (s *session) releaseLock() {
	if s.sessionLockFile != "" {
		os.Remove(s.sessionLockFile) //nolint:errcheck
		s.sessionLockFile = ""
	}
	if s.lockKey != "" {
		s.srv.unlock(s.lockKey)
		s.lockKey = ""
	}
	if s.limitIP != "" && s.srv.opts.ConnLimit != nil && s.userInfo != nil {
		s.srv.opts.ConnLimit.Release(s.userInfo.Username, s.limitIP)
		s.limitIP = ""
	}
	if s.box != nil {
		s.box.Close() //nolint:errcheck
		s.box = nil
	}
	if s.idx != nil {
		s.idx.Close() //nolint:errcheck
		s.idx = nil
	}
}

// acquireDotlock creates a dotlock file at $HOME/dovecot-pop3-session.lock.
// Returns true on success. A lock older than idleTimeout is considered stale
// and will be stolen (the session that held it is certainly gone by then).
func (s *session) acquireDotlock(home string) bool {
	if err := os.MkdirAll(home, 0o700); err != nil {
		slog.Warn("pop3: dotlock mkdir", "home", home, "err", err)
		return false
	}
	lockPath := filepath.Join(home, "dovecot-pop3-session.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		fmt.Fprintf(f, "%d\n", os.Getpid()) //nolint:errcheck
		f.Close()
		s.sessionLockFile = lockPath
		return true
	}
	if !errors.Is(err, os.ErrExist) {
		slog.Warn("pop3: dotlock create", "home", home, "err", err)
		return false
	}
	// Lock exists: treat as stale only if older than the idle timeout.
	info, err := os.Stat(lockPath)
	if err != nil || time.Since(info.ModTime()) < idleTimeout {
		return false
	}
	// Stale lock: remove and re-create atomically.
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false
	}
	f, err = os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	fmt.Fprintf(f, "%d\n", os.Getpid()) //nolint:errcheck
	f.Close()
	s.sessionLockFile = lockPath
	return true
}

func (s *session) setDeadline() {
	s.conn.SetDeadline(time.Now().Add(idleTimeout)) //nolint:errcheck
}

func (s *session) ok(msg string) {
	fmt.Fprintf(s.conn, "+OK %s\r\n", msg)
}

func (s *session) writeErr(msg string) {
	fmt.Fprintf(s.conn, "-ERR %s\r\n", msg)
}

func (s *session) writeDot() {
	s.conn.Write([]byte(".\r\n")) //nolint:errcheck
}

func (s *session) badCmd() {
	s.badCmds++
	s.writeErr("unknown command")
}

func (s *session) readLine() (string, error) {
	line, err := s.br.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func writeMultiLine(w io.Writer, data []byte) {
	lines := bytes.Split(data, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	for _, line := range lines {
		line = bytes.TrimRight(line, "\r")
		if bytes.HasPrefix(line, []byte(".")) {
			w.Write([]byte(".")) //nolint:errcheck
		}
		w.Write(line)           //nolint:errcheck
		w.Write([]byte("\r\n")) //nolint:errcheck
	}
	w.Write([]byte(".\r\n")) //nolint:errcheck
}

func writeTopLines(w io.Writer, data []byte, n int) {
	var headers, body []byte
	if idx := bytes.Index(data, []byte("\r\n\r\n")); idx >= 0 {
		headers = data[:idx]
		body = data[idx+4:]
	} else if idx := bytes.Index(data, []byte("\n\n")); idx >= 0 {
		headers = data[:idx]
		body = data[idx+2:]
	} else {
		headers = data
	}

	writeDotLines(w, headers)
	w.Write([]byte("\r\n")) //nolint:errcheck

	bodyLines := bytes.Split(body, []byte("\n"))
	if len(bodyLines) > 0 && len(bodyLines[len(bodyLines)-1]) == 0 {
		bodyLines = bodyLines[:len(bodyLines)-1]
	}
	for i, line := range bodyLines {
		if i >= n {
			break
		}
		line = bytes.TrimRight(line, "\r")
		if bytes.HasPrefix(line, []byte(".")) {
			w.Write([]byte(".")) //nolint:errcheck
		}
		w.Write(line)           //nolint:errcheck
		w.Write([]byte("\r\n")) //nolint:errcheck
	}
	w.Write([]byte(".\r\n")) //nolint:errcheck
}

func writeDotLines(w io.Writer, data []byte) {
	lines := bytes.Split(data, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	for _, line := range lines {
		line = bytes.TrimRight(line, "\r")
		if bytes.HasPrefix(line, []byte(".")) {
			w.Write([]byte(".")) //nolint:errcheck
		}
		w.Write(line)           //nolint:errcheck
		w.Write([]byte("\r\n")) //nolint:errcheck
	}
}
