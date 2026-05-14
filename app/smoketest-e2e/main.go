// smoketest-e2e drives a live yarilo instance end-to-end:
//
//  1. AUTH PLAIN over submission STARTTLS — verifies passdb wiring.
//  2. Deliver a test message via LMTP — verifies storage write path.
//  3. Read the delivered message via IMAPS (LOGIN, SELECT, FETCH).
//  4. Read the same message via POP3S (USER/PASS, STAT, RETR).
//
// Exit 0 on full success. Any failure prints details and exits 1.
package main

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	imapclient "github.com/emersion/go-imap/v2/imapclient"
)

var (
	flagHost           = flag.String("host", "127.0.0.1", "yarilo host")
	flagUser           = flag.String("user", "alice@smoke.local", "mailbox login")
	flagPass           = flag.String("pass", "wonderland", "password (plain)")
	flagSubmissionPort = flag.Int("submission-port", 9587, "submission (STARTTLS)")
	flagLMTPPort       = flag.Int("lmtp-port", 9024, "LMTP (plain TCP)")
	flagIMAPSPort      = flag.Int("imaps-port", 9993, "IMAPS")
	flagPOP3SPort      = flag.Int("pop3s-port", 9995, "POP3S")
	flagTimeout        = flag.Duration("timeout", 10*time.Second, "per-step timeout")
	flagInsecure       = flag.Bool("insecure", true, "skip TLS verify (self-signed test cert)")
)

func main() {
	flag.Parse()

	steps := []struct {
		name string
		fn   func() error
	}{
		{"submission AUTH PLAIN over STARTTLS", checkSubmissionAuth},
		{"LMTP deliver to mailbox", deliverLMTP},
		{"IMAPS LOGIN + SELECT INBOX + FETCH", readViaIMAPS},
		{"POP3S USER/PASS + STAT + RETR", readViaPOP3S},
	}

	failed := false
	for _, s := range steps {
		err := s.fn()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[FAIL] %s: %v\n", s.name, err)
			failed = true
			continue
		}
		fmt.Printf("[PASS] %s\n", s.name)
	}
	if failed {
		os.Exit(1)
	}
}

// ---- step 1: submission AUTH ----------------------------------------------

func checkSubmissionAuth() error {
	addr := fmt.Sprintf("%s:%d", *flagHost, *flagSubmissionPort)
	conn, err := net.DialTimeout("tcp", addr, *flagTimeout)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(*flagTimeout))

	br := bufio.NewReader(conn)
	if err := expectCode(br, "220"); err != nil {
		return err
	}
	if err := smtpCmd(conn, br, "EHLO smoketest", "250"); err != nil {
		return err
	}
	if err := smtpCmd(conn, br, "STARTTLS", "220"); err != nil {
		return err
	}
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         *flagHost,
		InsecureSkipVerify: *flagInsecure, //nolint:gosec
		NextProtos:         []string{"smtp"},
	})
	if err := tlsConn.HandshakeContext(deadlineCtx(*flagTimeout)); err != nil {
		return fmt.Errorf("starttls handshake: %w", err)
	}
	br = bufio.NewReader(tlsConn)
	if err := smtpCmd(tlsConn, br, "EHLO smoketest", "250"); err != nil {
		return err
	}
	auth := base64.StdEncoding.EncodeToString([]byte("\x00" + *flagUser + "\x00" + *flagPass))
	if err := smtpCmd(tlsConn, br, "AUTH PLAIN "+auth, "235"); err != nil {
		return err
	}
	_ = smtpCmdNoCheck(tlsConn, br, "QUIT")
	return nil
}

// ---- step 2: LMTP deliver --------------------------------------------------

const testSubject = "yarilo-smoketest"

func deliverLMTP() error {
	addr := fmt.Sprintf("%s:%d", *flagHost, *flagLMTPPort)
	conn, err := net.DialTimeout("tcp", addr, *flagTimeout)
	if err != nil {
		return fmt.Errorf("dial lmtp: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(*flagTimeout))

	br := bufio.NewReader(conn)
	if err := expectCode(br, "220"); err != nil {
		return err
	}
	if err := smtpCmd(conn, br, "LHLO smoketest", "250"); err != nil {
		return err
	}
	if err := smtpCmd(conn, br, "MAIL FROM:<sender@external>", "250"); err != nil {
		return err
	}
	if err := smtpCmd(conn, br, "RCPT TO:<"+*flagUser+">", "250"); err != nil {
		return err
	}
	if err := smtpCmd(conn, br, "DATA", "354"); err != nil {
		return err
	}
	body := fmt.Sprintf("From: sender@external\r\nTo: %s\r\nSubject: %s\r\n\r\nHello %s.\r\n.\r\n",
		*flagUser, testSubject, *flagUser)
	if _, err := conn.Write([]byte(body)); err != nil {
		return fmt.Errorf("write body: %w", err)
	}
	if err := expectCode(br, "250"); err != nil {
		return err
	}
	_ = smtpCmdNoCheck(conn, br, "QUIT")
	return nil
}

// ---- step 3: IMAPS read ----------------------------------------------------

func readViaIMAPS() error {
	addr := fmt.Sprintf("%s:%d", *flagHost, *flagIMAPSPort)
	tlsCfg := &tls.Config{
		ServerName:         *flagHost,
		InsecureSkipVerify: *flagInsecure, //nolint:gosec
		NextProtos:         []string{"imap"},
	}
	c, err := imapclient.DialTLS(addr, &imapclient.Options{TLSConfig: tlsCfg})
	if err != nil {
		return fmt.Errorf("imap dial: %w", err)
	}
	defer c.Close() //nolint:errcheck

	if err := c.Login(*flagUser, *flagPass).Wait(); err != nil {
		return fmt.Errorf("imap login: %w", err)
	}
	mb, err := c.Select("INBOX", nil).Wait()
	if err != nil {
		return fmt.Errorf("imap select: %w", err)
	}
	if mb.NumMessages < 1 {
		return fmt.Errorf("INBOX is empty — expected the LMTP-delivered message")
	}
	return nil
}

// ---- step 4: POP3S read ----------------------------------------------------

func readViaPOP3S() error {
	addr := fmt.Sprintf("%s:%d", *flagHost, *flagPOP3SPort)
	tlsCfg := &tls.Config{
		ServerName:         *flagHost,
		InsecureSkipVerify: *flagInsecure, //nolint:gosec
		NextProtos:         []string{"pop3"},
	}
	conn, err := tls.Dial("tcp", addr, tlsCfg)
	if err != nil {
		return fmt.Errorf("pop3 dial: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(*flagTimeout))

	br := bufio.NewReader(conn)
	line, err := readLine(br)
	if err != nil || !strings.HasPrefix(line, "+OK") {
		return fmt.Errorf("pop3 greeting: %q (err=%v)", line, err)
	}
	if err := pop3Cmd(conn, br, "USER "+*flagUser); err != nil {
		return err
	}
	if err := pop3Cmd(conn, br, "PASS "+*flagPass); err != nil {
		return err
	}
	statLine, err := pop3CmdLine(conn, br, "STAT")
	if err != nil {
		return err
	}
	// STAT response: "+OK 1 1234"
	parts := strings.Fields(statLine)
	if len(parts) < 2 || parts[0] != "+OK" {
		return fmt.Errorf("pop3 stat malformed: %q", statLine)
	}
	if parts[1] == "0" {
		return fmt.Errorf("pop3 inbox empty (STAT: %q)", statLine)
	}
	// RETR 1 — read full message, look for our test subject.
	if _, err := io.WriteString(conn, "RETR 1\r\n"); err != nil {
		return err
	}
	first, err := readLine(br)
	if err != nil || !strings.HasPrefix(first, "+OK") {
		return fmt.Errorf("pop3 retr 1: %q (err=%v)", first, err)
	}
	var sawSubject bool
	for {
		l, err := readLine(br)
		if err != nil {
			return fmt.Errorf("pop3 retr stream: %w", err)
		}
		if l == "." {
			break
		}
		if strings.Contains(l, testSubject) {
			sawSubject = true
		}
	}
	if !sawSubject {
		return fmt.Errorf("pop3 retr did not include test subject %q", testSubject)
	}
	_ = pop3Cmd(conn, br, "QUIT")
	return nil
}

// ---- helpers ---------------------------------------------------------------

func smtpCmd(w io.Writer, r *bufio.Reader, line, wantCode string) error {
	if _, err := io.WriteString(w, line+"\r\n"); err != nil {
		return fmt.Errorf("write %q: %w", line, err)
	}
	return expectCode(r, wantCode)
}

func smtpCmdNoCheck(w io.Writer, r *bufio.Reader, line string) error {
	if _, err := io.WriteString(w, line+"\r\n"); err != nil {
		return err
	}
	_, _ = readLine(r)
	return nil
}

func expectCode(r *bufio.Reader, wantCode string) error {
	for {
		line, err := readLine(r)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		if !strings.HasPrefix(line, wantCode) {
			return fmt.Errorf("got %q, want %q", line, wantCode)
		}
		// Multi-line: "250-" continues, "250 " ends.
		if len(line) > 3 && line[3] == '-' {
			continue
		}
		return nil
	}
}

func pop3Cmd(w io.Writer, r *bufio.Reader, line string) error {
	_, err := pop3CmdLine(w, r, line)
	return err
}

func pop3CmdLine(w io.Writer, r *bufio.Reader, line string) (string, error) {
	if _, err := io.WriteString(w, line+"\r\n"); err != nil {
		return "", err
	}
	resp, err := readLine(r)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(resp, "+OK") {
		return "", fmt.Errorf("%s -> %q", line, resp)
	}
	return resp, nil
}

func readLine(r *bufio.Reader) (string, error) {
	s, err := r.ReadString('\n')
	return strings.TrimRight(s, "\r\n"), err
}

func deadlineCtx(d time.Duration) interface {
	Deadline() (time.Time, bool)
	Done() <-chan struct{}
	Err() error
	Value(any) any
} {
	// crypto/tls HandshakeContext needs context.Context; we use a tiny custom one
	// so this binary keeps a tight dep tree.
	return &tinyCtx{deadline: time.Now().Add(d)}
}

type tinyCtx struct {
	deadline time.Time
	done     chan struct{}
}

func (c *tinyCtx) Deadline() (time.Time, bool) { return c.deadline, true }
func (c *tinyCtx) Done() <-chan struct{}       { return c.done }
func (c *tinyCtx) Err() error                  { return nil }
func (c *tinyCtx) Value(any) any               { return nil }
