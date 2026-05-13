package pop3

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	"github.com/0kaba0hub/yarilo/internal/xclient"
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
)

type session struct {
	srv      *Server
	conn     net.Conn
	br       *bufio.Reader
	state    pop3State
	onTLS    bool
	remoteIP net.IP // updated by XCLIENT

	// set after successful login
	lockKey  string
	username string
	folder   *mailbox.Folder
	msgs     []*mailbox.MessageMeta
	deleted  []bool

	badCmds int
}

func (s *Server) newSession(conn net.Conn) *session {
	ip := net.IPv4zero
	if tcp, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		ip = tcp.IP
	}
	return &session{
		srv:      s,
		conn:     conn,
		br:       bufio.NewReader(conn),
		state:    stateAuth,
		remoteIP: ip,
	}
}

func (s *session) serve() {
	defer s.conn.Close()
	defer s.releaseLock()

	s.setDeadline()
	s.ok("yarilo POP3 server ready")

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

// ---- AUTH state -----------------------------------------------------------

func (s *session) handleAuth(cmd, arg string) {
	switch cmd {
	case "CAPA":
		s.sendCapa()
	case "USER":
		s.cmdUser(arg)
	case "PASS":
		s.cmdPass(arg)
	case "QUIT":
		s.ok("yarilo signing off")
		s.state = stateDone
	case "STLS":
		s.cmdSTLS()
	case "XCLIENT":
		s.cmdXClient(arg)
	default:
		s.badCmd()
	}
}

func (s *session) cmdUser(arg string) {
	if arg == "" {
		s.writeErr("missing username")
		return
	}
	s.username = arg
	s.ok("send PASS")
}

func (s *session) cmdPass(arg string) {
	if s.username == "" {
		s.writeErr("USER required before PASS")
		return
	}
	if s.srv.opts.DisablePlainAuth && !s.onTLS {
		s.writeErr("plaintext authentication disabled, use STLS first")
		s.username = ""
		return
	}

	res, err := s.srv.opts.Auth.Authenticate(s.username, arg, "pop3")
	if err != nil || res == nil || res.Result != protocol.AuthOK {
		slog.Info("pop3: auth failed", "user", s.username, "remoteIP", s.remoteIP)
		s.writeErr("authentication failed")
		s.username = ""
		return
	}
	s.username = res.Username

	if !s.srv.tryLock(s.username) {
		s.writeErr("mailbox already in use, try again later")
		s.username = ""
		return
	}
	s.lockKey = s.username

	if err := s.loadMailbox(); err != nil {
		s.writeErr("internal error")
		s.srv.unlock(s.lockKey)
		s.lockKey = ""
		return
	}
	slog.Info("pop3: login", "user", s.username, "messages", len(s.msgs))
	s.state = stateTrans
	s.ok(fmt.Sprintf("logged in, %d messages", len(s.msgs)))
}

func (s *session) loadMailbox() error {
	if err := s.srv.opts.Mailbox.Init(s.username); err != nil {
		slog.Error("pop3: mailbox init", "user", s.username, "err", err)
		return err
	}
	folder, err := s.srv.opts.Index.OpenFolder(s.username, "INBOX", uint32(time.Now().Unix()))
	if err != nil {
		slog.Error("pop3: open folder", "user", s.username, "err", err)
		return err
	}
	msgs, err := s.srv.opts.Index.GetMessages(folder.ID, mailbox.SeqSet{})
	if err != nil {
		slog.Error("pop3: get messages", "user", s.username, "err", err)
		return err
	}
	s.folder = folder
	s.msgs = msgs
	s.deleted = make([]bool, len(msgs))
	return nil
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
	s.username = "" // RFC 2595 §4: reset state after TLS upgrade
}

func (s *session) cmdXClient(arg string) {
	if !s.srv.opts.XClient || !s.isTrusted() {
		// Always respond +OK to avoid leaking XCLIENT support status.
		s.ok("XCLIENT ignored")
		return
	}
	attrs, err := xclient.Parse("XCLIENT " + arg)
	if err == nil && attrs.Addr != "" {
		if ip := net.ParseIP(attrs.Addr); ip != nil {
			s.remoteIP = ip
		}
	}
	s.ok("XCLIENT accepted")
}

func (s *session) isTrusted() bool {
	nets := s.srv.opts.XClientTrustedNets
	if len(nets) == 0 {
		return false
	}
	for _, n := range nets {
		if n.Contains(s.remoteIP) {
			return true
		}
	}
	return false
}

// ---- TRANSACTION state ----------------------------------------------------

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
		s.ok(fmt.Sprintf("%d %d", idx+1, s.msgs[idx].Size))
		return
	}
	count, total := s.countActive()
	s.ok(fmt.Sprintf("%d messages (%d octets)", count, total))
	for i, m := range s.msgs {
		if !s.deleted[i] {
			fmt.Fprintf(s.conn, "%d %d\r\n", i+1, m.Size)
		}
	}
	s.writeDot()
}

func (s *session) cmdRetr(arg string) {
	idx, ok := s.parseMsgNum(arg)
	if !ok {
		return
	}
	m := s.msgs[idx]
	rc, err := s.srv.opts.Mailbox.Fetch(s.username, "INBOX", m.Filename)
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
	for i := range s.deleted {
		s.deleted[i] = false
	}
	count, _ := s.countActive()
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
	rc, err := s.srv.opts.Mailbox.Fetch(s.username, "INBOX", m.Filename)
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
		s.ok(fmt.Sprintf("%d %s", idx+1, s.uidl(s.msgs[idx])))
		return
	}
	s.ok("")
	for i, m := range s.msgs {
		if !s.deleted[i] {
			fmt.Fprintf(s.conn, "%d %s\r\n", i+1, s.uidl(m))
		}
	}
	s.writeDot()
}

func (s *session) cmdQuit() {
	var errCount int
	for i, m := range s.msgs {
		if !s.deleted[i] {
			continue
		}
		if err := s.srv.opts.Mailbox.Remove(s.username, "INBOX", m.Filename); err != nil {
			slog.Error("pop3: remove", "uid", m.UID, "err", err)
			errCount++
		} else {
			s.srv.opts.Index.ExpungeMessage(s.folder.ID, m.UID) //nolint:errcheck
		}
	}
	if errCount > 0 {
		s.writeErr(fmt.Sprintf("%d message(s) could not be deleted", errCount))
	} else {
		s.ok("yarilo signing off")
	}
	s.state = stateDone
}

// ---- helpers --------------------------------------------------------------

func (s *session) sendCapa() {
	s.ok("capability list follows")
	fmt.Fprintf(s.conn, "TOP\r\nUIDL\r\nUSER\r\nRESP-CODES\r\nPIPELINING\r\nAUTH-RESP-CODE\r\nSASL PLAIN\r\n")
	if s.srv.opts.TLSConfig != nil && !s.onTLS {
		fmt.Fprintf(s.conn, "STLS\r\n")
	}
	s.writeDot()
}

// uidl returns the UIDL string for a message: "<uid>.<uidvalidity>".
func (s *session) uidl(m *mailbox.MessageMeta) string {
	return fmt.Sprintf("%d.%d", m.UID, s.folder.UIDValidity)
}

func (s *session) countActive() (count int, total int64) {
	for i, m := range s.msgs {
		if !s.deleted[i] {
			count++
			total += int64(m.Size)
		}
	}
	return count, total
}

// parseMsgNum parses a 1-based message number and returns the 0-based index.
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
	if s.lockKey != "" {
		s.srv.unlock(s.lockKey)
		s.lockKey = ""
	}
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

// writeMultiLine writes data as a dot-stuffed POP3 multi-line response,
// terminated by ".\r\n".
func writeMultiLine(w io.Writer, data []byte) {
	lines := bytes.Split(data, []byte("\n"))
	// Drop trailing empty element produced by a final newline.
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

// writeTopLines writes the headers of data plus the first n body lines,
// dot-stuffed, terminated by ".\r\n".
func writeTopLines(w io.Writer, data []byte, n int) {
	// Locate the blank line between headers and body.
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
	w.Write([]byte("\r\n")) //nolint:errcheck — blank line after headers

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
