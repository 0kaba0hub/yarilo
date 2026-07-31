package main

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"
)

// ── ManageSieve connection (STARTTLS-aware) ───────────────────────────────

// msieveDial opens a plain TCP connection to the ManageSieve port, reads the
// pre-auth capability block, and performs a STARTTLS upgrade when advertised.
// Returns the (possibly upgraded) connection with the greeting already consumed.
func msieveDial() (net.Conn, error) {
	addr := net.JoinHostPort(*flagHost, *flagManageSievePort)
	conn, err := net.DialTimeout("tcp", addr, *flagTimeout)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(*flagTimeout)) //nolint:errcheck

	starttls, err := msieveReadCapabilities(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("capabilities: %w", err)
	}
	if !starttls {
		return conn, nil
	}

	fmt.Fprintf(conn, "STARTTLS\r\n") //nolint:errcheck
	line, err := readLine(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("STARTTLS response: %w", err)
	}
	if !strings.HasPrefix(line, "OK") {
		conn.Close()
		return nil, fmt.Errorf("STARTTLS rejected: %q", line)
	}
	tlsCfg := &tls.Config{
		ServerName:         *flagHost,
		InsecureSkipVerify: *flagInsecure, //nolint:gosec
	}
	tlsConn := tls.Client(conn, tlsCfg)
	if err := tlsConn.Handshake(); err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("STARTTLS handshake: %w", err)
	}
	tlsConn.SetDeadline(time.Now().Add(*flagTimeout)) //nolint:errcheck
	// Re-read post-TLS capabilities.
	if _, err := msieveReadCapabilities(tlsConn); err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("post-TLS capabilities: %w", err)
	}
	return tlsConn, nil
}

// msieveReadCapabilities reads the ManageSieve capability block until OK.
// Returns whether STARTTLS was advertised in the block.
func msieveReadCapabilities(conn net.Conn) (starttls bool, err error) {
	for {
		line, err := readLine(conn)
		if err != nil {
			return false, err
		}
		if strings.HasPrefix(line, "OK") {
			return starttls, nil
		}
		if strings.HasPrefix(line, "NO") || strings.HasPrefix(line, "BYE") {
			return false, fmt.Errorf("server error: %q", line)
		}
		if strings.Contains(line, "STARTTLS") {
			starttls = true
		}
	}
}

// msieveAuth performs AUTHENTICATE PLAIN on an already-connected (post-greeting) conn.
func msieveAuth(conn net.Conn, user, pass string) error {
	creds := base64.StdEncoding.EncodeToString([]byte("\x00" + user + "\x00" + pass))
	fmt.Fprintf(conn, "AUTHENTICATE \"PLAIN\" \"%s\"\r\n", creds) //nolint:errcheck
	if err := msieveDrainUntilOK(conn); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	return nil
}

// ── ManageSieve script management ─────────────────────────────────────────

func msieveSetActive(script string) error {
	conn, err := msieveDial()
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := msieveAuth(conn, *flagManageSieveUser, *flagManageSievePass); err != nil {
		return err
	}
	fmt.Fprintf(conn, "PUTSCRIPT %q {%d+}\r\n%s", sieveScriptNameConst, len(script), script) //nolint:errcheck
	line, err := readLine(conn)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(line, "OK") {
		return fmt.Errorf("PUTSCRIPT: %q", line)
	}
	fmt.Fprintf(conn, "SETACTIVE %q\r\n", sieveScriptNameConst) //nolint:errcheck
	line, err = readLine(conn)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(line, "OK") {
		return fmt.Errorf("SETACTIVE: %q", line)
	}
	fmt.Fprintf(conn, "LOGOUT\r\n") //nolint:errcheck
	return nil
}

func msieveDeactivateAndDelete(name string) {
	conn, err := msieveDial()
	if err != nil {
		return
	}
	defer conn.Close()
	if err := msieveAuth(conn, *flagManageSieveUser, *flagManageSievePass); err != nil {
		return
	}
	fmt.Fprintf(conn, "SETACTIVE \"\"\r\n")        //nolint:errcheck
	readLine(conn)                                 //nolint:errcheck
	fmt.Fprintf(conn, "DELETESCRIPT %q\r\n", name) //nolint:errcheck
	readLine(conn)                                 //nolint:errcheck
	fmt.Fprintf(conn, "LOGOUT\r\n")                //nolint:errcheck
}

// ── SMTP injector (via MX) ────────────────────────────────────────────────

func lmtpSend(id, from, to, subject, body string) error {
	addr := net.JoinHostPort(smtpHost(), *flagSieveSMTPPort)
	conn, err := net.DialTimeout("tcp", addr, *flagTimeout)
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(*flagTimeout)) //nolint:errcheck
	r := bufio.NewReader(conn)

	readResp := func() (string, error) {
		var last string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return "", err
			}
			last = strings.TrimRight(line, "\r\n")
			if len(line) < 4 || line[3] != '-' {
				return last, nil
			}
		}
	}
	cmd := func(c string) (string, error) {
		fmt.Fprintf(conn, "%s\r\n", c)
		return readResp()
	}

	if _, err := readResp(); err != nil {
		return fmt.Errorf("greeting: %w", err)
	}
	if resp, err := cmd("EHLO smoketest"); err != nil || !strings.HasPrefix(resp, "250") {
		return fmt.Errorf("EHLO: %s %v", resp, err)
	}
	if resp, err := cmd("MAIL FROM:<" + from + ">"); err != nil || !strings.HasPrefix(resp, "250") {
		return fmt.Errorf("MAIL FROM: %s %v", resp, err)
	}
	if resp, err := cmd("RCPT TO:<" + to + ">"); err != nil {
		return fmt.Errorf("RCPT TO: %w", err)
	} else if !strings.HasPrefix(resp, "250") {
		return fmt.Errorf("RCPT TO: %s", resp)
	}
	if resp, err := cmd("DATA"); err != nil || !strings.HasPrefix(resp, "354") {
		return fmt.Errorf("DATA: %s %v", resp, err)
	}
	ts := time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 +0000")
	fmt.Fprintf(conn, "Message-ID: <%s>\r\nDate: %s\r\nFrom: <%s>\r\nTo: <%s>\r\nSubject: %s\r\n\r\n%s\r\n.\r\n",
		id, ts, from, to, subject, body)
	if _, err := readResp(); err != nil {
		return fmt.Errorf("end-of-data: %w", err)
	}
	cmd("QUIT") //nolint:errcheck
	return nil
}

// ── minimal IMAP4rev1 client ───────────────────────────────────────────────

type imapClient struct {
	conn net.Conn
	r    *bufio.Reader
	seq  int
}

func imapDial() (*imapClient, error) {
	tlsCfg := &tls.Config{InsecureSkipVerify: *flagInsecure, ServerName: imapHost()} //nolint:gosec
	addr := net.JoinHostPort(imapHost(), *flagIMAPSPort)
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: *flagTimeout}, "tcp", addr, tlsCfg)
	if err != nil {
		return nil, err
	}
	c := &imapClient{conn: conn, r: bufio.NewReader(conn)}
	conn.SetDeadline(time.Now().Add(*flagTimeout)) //nolint:errcheck
	if _, err := c.r.ReadString('\n'); err != nil {
		conn.Close()
		return nil, fmt.Errorf("greeting: %w", err)
	}
	return c, nil
}

func (c *imapClient) close() { c.conn.Close() }

func (c *imapClient) cmd(command string) ([]string, error) {
	c.seq++
	tag := fmt.Sprintf("S%04d", c.seq)
	// Set the read deadline per command from imap-read-timeout, above the server
	// fts catch-up budget, so a legitimate index wait (UID SEARCH under load) is
	// not misread as an i/o timeout (#934). A caller may have set a shorter
	// deadline; this overrides it for the whole command.
	start := time.Now()
	c.conn.SetDeadline(start.Add(*flagIMAPReadTimeout)) //nolint:errcheck
	defer func() {
		// Keep slowness visible without failing the run: a command that outlasts
		// the ordinary per-check timeout was the server legitimately waiting on
		// the FTS index, not a fault.
		if d := time.Since(start); d > *flagTimeout {
			slog.Warn("smoke: slow IMAP command", "command", command, "took", d.Round(time.Second).String())
		}
	}()
	fmt.Fprintf(c.conn, "%s %s\r\n", tag, command)
	var untagged []string
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, tag+" OK") {
			return untagged, nil
		}
		if strings.HasPrefix(line, tag+" ") {
			return untagged, fmt.Errorf("%s", line)
		}
		untagged = append(untagged, line)
	}
}

func (c *imapClient) login(user, pass string) error {
	_, err := c.cmd(fmt.Sprintf("LOGIN %q %q", user, pass))
	return err
}

func (c *imapClient) selectFolder(folder string) (int, error) {
	lines, err := c.cmd(fmt.Sprintf("SELECT %q", folder))
	if err != nil {
		return 0, err
	}
	for _, l := range lines {
		var n int
		if _, err2 := fmt.Sscanf(l, "* %d EXISTS", &n); err2 == nil {
			return n, nil
		}
	}
	return 0, nil
}

func (c *imapClient) uidSearch(criteria string) ([]string, error) {
	lines, err := c.cmd("UID SEARCH " + criteria)
	if err != nil {
		return nil, err
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "* SEARCH") {
			parts := strings.Fields(l)
			if len(parts) <= 2 {
				return nil, nil
			}
			return parts[2:], nil
		}
	}
	return nil, nil
}

func (c *imapClient) deleteUIDs(uids []string) error {
	if len(uids) == 0 {
		return nil
	}
	if _, err := c.cmd(fmt.Sprintf("UID STORE %s +FLAGS.SILENT (\\Deleted)", strings.Join(uids, ","))); err != nil {
		return err
	}
	_, err := c.cmd("EXPUNGE")
	return err
}

// clearInbox deletes all messages from INBOX so that inboxWaitByUID
// gets a clean UIDNEXT unaffected by leftover messages from previous tests.
func clearInbox(user, pass string) {
	c, err := imapDial()
	if err != nil {
		return
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return
	}
	if _, err := c.selectFolder("INBOX"); err != nil {
		return
	}
	uids, err := c.uidSearch("ALL")
	if err != nil || len(uids) == 0 {
		return
	}
	c.deleteUIDs(uids) //nolint:errcheck
}

func (c *imapClient) deleteFolder(folder string) {
	c.cmd(fmt.Sprintf("DELETE %q", folder)) //nolint:errcheck
}

// ── per-test helpers ───────────────────────────────────────────────────────

func sieveInject(script, from, to, id, subject, body string) error {
	if err := msieveSetActive(script); err != nil {
		return fmt.Errorf("msieve: %w", err)
	}
	err := lmtpSend(id, from, to, subject, body)
	return err
}

func createFolder(user, pass, folder string) error {
	c, err := imapDial()
	if err != nil {
		return fmt.Errorf("imap dial: %w", err)
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return fmt.Errorf("imap login: %w", err)
	}
	if _, err := c.cmd(fmt.Sprintf("CREATE %q", folder)); err != nil {
		return fmt.Errorf("CREATE %q: %w", folder, err)
	}
	return nil
}

func checkFolder(user, pass, folder string) error {
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for {
		c, err := imapDial()
		if err != nil {
			return fmt.Errorf("imap dial: %w", err)
		}
		loginErr := c.login(user, pass)
		if loginErr != nil {
			c.close()
			return fmt.Errorf("imap login: %w", loginErr)
		}
		exists, selErr := c.selectFolder(folder)
		switch {
		case selErr != nil:
			// fileinto :create is asynchronous: lmtpSend returns once the relay
			// accepts the message, before delivery+Sieve create the folder. A
			// SELECT that beats it returns NONEXISTENT — keep polling, don't fail.
			lastErr = selErr
		case exists >= 1:
			uids, _ := c.uidSearch("ALL")
			c.deleteUIDs(uids) //nolint:errcheck
			c.deleteFolder(folder)
			c.close()
			return nil
		}
		c.close()
		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("folder %q never became selectable: %w", folder, lastErr)
			}
			return fmt.Errorf("expected 1 message in %q, got 0 (timed out)", folder)
		}
		time.Sleep(1 * time.Second)
	}
}

// checkAbsentInInbox does a single IMAP SEARCH and returns an error if any
// message matching subjectToken is found. Used for negative assertions (reject,
// ereject) where a 30s poll loop is wrong — absence should be confirmed once.
func checkAbsentInInbox(user, pass, subjectToken string) error {
	c, err := imapDial()
	if err != nil {
		return err
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return err
	}
	if _, err := c.selectFolder("INBOX"); err != nil {
		return err
	}
	uids, err := c.uidSearch(fmt.Sprintf("SUBJECT %q", subjectToken))
	if err != nil {
		return err
	}
	if len(uids) > 0 {
		c.deleteUIDs(uids) //nolint:errcheck
		return fmt.Errorf("found %d unexpected message(s) with subject %q in INBOX", len(uids), subjectToken)
	}
	return nil
}

func cleanInboxBySubject(user, pass, subjectToken string) (int, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		c, err := imapDial()
		if err != nil {
			return 0, err
		}
		if err := c.login(user, pass); err != nil {
			c.close()
			return 0, err
		}
		if _, err := c.selectFolder("INBOX"); err != nil {
			c.close()
			return 0, err
		}
		uids, err := c.uidSearch(fmt.Sprintf("SUBJECT %q", subjectToken))
		if err != nil {
			c.close()
			return 0, err
		}
		if len(uids) > 0 {
			c.deleteUIDs(uids) //nolint:errcheck
			c.close()
			return len(uids), nil
		}
		c.close()
		if time.Now().After(deadline) {
			return 0, nil
		}
		time.Sleep(1 * time.Second)
	}
}

func uniqueID() string {
	return fmt.Sprintf("sieve-smoke-%d@test", time.Now().UnixNano())
}

const sieveScriptNameConst = "smoke-sieve-plugins"

// ── plugin tests ───────────────────────────────────────────────────────────

func testSieveFileinto(user, pass, to string) error {
	clearInbox(user, pass)
	folder := "sieve-test-fileinto"
	if err := createFolder(user, pass, folder); err != nil {
		return fmt.Errorf("pre-create: %w", err)
	}
	script := fmt.Sprintf("require \"fileinto\";\nfileinto %q;\n", folder)
	if err := sieveInject(script, "sender@test.invalid", to, uniqueID(), "fileinto test", "body"); err != nil {
		return err
	}
	return checkFolder(user, pass, folder)
}

func testSieveMailbox(user, pass, to string) error {
	clearInbox(user, pass)
	folder := "sieve-test-mailbox"
	// Delete the folder if it survived a previous run so :create starts fresh.
	func() {
		c, err := imapDial()
		if err != nil {
			return
		}
		defer c.close()
		if err := c.login(user, pass); err != nil {
			return
		}
		c.deleteFolder(folder)
	}()
	script := fmt.Sprintf("require [\"fileinto\",\"mailbox\"];\nfileinto :create %q;\n", folder)
	if err := sieveInject(script, "sender@test.invalid", to, uniqueID(), "mailbox:create test", "body"); err != nil {
		return err
	}
	return checkFolder(user, pass, folder)
}

func testSieveImap4flags(user, pass, to string) error {
	clearInbox(user, pass)
	id := uniqueID()
	script := "require \"imap4flags\";\naddflag \"\\\\Flagged\";\nkeep;\n"
	if err := msieveSetActive(script); err != nil {
		return fmt.Errorf("msieve: %w", err)
	}
	if err := lmtpSend(id, "sender@test.invalid", to, "imap4flags test", "body"); err != nil {
		return err
	}
	time.Sleep(2 * time.Second)
	c, err := imapDial()
	if err != nil {
		return err
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return err
	}
	if _, err := c.selectFolder("INBOX"); err != nil {
		return err
	}
	uids, err := c.uidSearch(fmt.Sprintf("HEADER Message-ID \"<%s>\"", id))
	if err != nil {
		return err
	}
	if len(uids) == 0 {
		return fmt.Errorf("message not found in INBOX")
	}
	lines, err := c.cmd(fmt.Sprintf("UID FETCH %s (FLAGS)", uids[0]))
	if err != nil {
		return err
	}
	flagged := false
	for _, l := range lines {
		if strings.Contains(l, "\\Flagged") {
			flagged = true
		}
	}
	c.deleteUIDs(uids) //nolint:errcheck
	if !flagged {
		return fmt.Errorf("message is not \\Flagged")
	}
	return nil
}

func testSieveBody(user, pass, to string) error {
	clearInbox(user, pass)
	folder := "sieve-test-body"
	if err := createFolder(user, pass, folder); err != nil {
		return fmt.Errorf("pre-create: %w", err)
	}
	token := fmt.Sprintf("XBODY%d", time.Now().UnixNano())
	script := fmt.Sprintf("require [\"fileinto\",\"body\"];\nif body :contains %q { fileinto %q; }\n", token, folder)
	if err := sieveInject(script, "sender@test.invalid", to, uniqueID(), "body test", token); err != nil {
		return err
	}
	return checkFolder(user, pass, folder)
}

func testSieveEnvelope(user, pass, to string) error {
	clearInbox(user, pass)
	folder := "sieve-test-envelope"
	if err := createFolder(user, pass, folder); err != nil {
		return fmt.Errorf("pre-create: %w", err)
	}
	script := fmt.Sprintf("require [\"fileinto\",\"envelope\"];\nif envelope \"to\" %q { fileinto %q; }\n", to, folder)
	if err := sieveInject(script, "sender@test.invalid", to, uniqueID(), "envelope test", "body"); err != nil {
		return err
	}
	return checkFolder(user, pass, folder)
}

func testSieveVariables(user, pass, to string) error {
	clearInbox(user, pass)
	folder := "sieve-test-variables"
	if err := createFolder(user, pass, folder); err != nil {
		return fmt.Errorf("pre-create: %w", err)
	}
	script := fmt.Sprintf("require [\"fileinto\",\"variables\"];\nset \"f\" %q;\nfileinto \"${f}\";\n", folder)
	if err := sieveInject(script, "sender@test.invalid", to, uniqueID(), "variables test", "body"); err != nil {
		return err
	}
	return checkFolder(user, pass, folder)
}

func testSieveReject(user, pass, to string) error {
	clearInbox(user, pass)
	subject := "reject-" + uniqueID()
	script := "require \"reject\";\nreject \"smoke test reject\";\n"
	if err := sieveInject(script, "", to, uniqueID(), subject, "body"); err != nil {
		return err
	}
	// MX accepts the message (250), yarilo-lmtp rejects it async — message must NOT land in INBOX.
	// Single check after a short wait: no poll loop needed for a negative assertion.
	time.Sleep(5 * time.Second)
	if err := checkAbsentInInbox(user, pass, subject); err != nil {
		return fmt.Errorf("reject: %w", err)
	}
	return nil
}

func testSieveEreject(user, pass, to string) error {
	clearInbox(user, pass)
	subject := "ereject-" + uniqueID()
	script := "require \"ereject\";\nereject \"smoke test ereject\";\n"
	if err := sieveInject(script, "", to, uniqueID(), subject, "body"); err != nil {
		return err
	}
	// Same as reject — single check after wait, no poll loop.
	time.Sleep(5 * time.Second)
	if err := checkAbsentInInbox(user, pass, subject); err != nil {
		return fmt.Errorf("ereject: %w", err)
	}
	return nil
}

func testSieveDuplicate(user, pass, to string) error {
	clearInbox(user, pass)
	folder := "sieve-test-duplicate"
	if err := createFolder(user, pass, folder); err != nil {
		return fmt.Errorf("pre-create: %w", err)
	}
	fixedID := fmt.Sprintf("sieve-dedup-fixed-%d@test", time.Now().UnixNano())
	script := fmt.Sprintf("require [\"fileinto\",\"duplicate\"];\nif not duplicate { fileinto %q; }\n", folder)
	if err := msieveSetActive(script); err != nil {
		return fmt.Errorf("msieve: %w", err)
	}
	for i := 0; i < 2; i++ {
		if err := lmtpSend(fixedID, "sender@test.invalid", to, "dup test", "body"); err != nil {
			return fmt.Errorf("inject %d: %w", i+1, err)
		}
	}
	// Clean up duplicate that landed in INBOX (second copy, non-duplicate path)
	c, err := imapDial()
	if err != nil {
		return err
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return err
	}
	if _, err := c.selectFolder("INBOX"); err == nil {
		uids, _ := c.uidSearch(fmt.Sprintf("HEADER Message-ID \"<%s>\"", fixedID))
		c.deleteUIDs(uids) //nolint:errcheck
	}
	return checkFolder(user, pass, folder)
}

func testSieveRelational(user, pass, to string) error {
	clearInbox(user, pass)
	folder := "sieve-test-relational"
	if err := createFolder(user, pass, folder); err != nil {
		return fmt.Errorf("pre-create: %w", err)
	}
	script := fmt.Sprintf(
		"require [\"fileinto\",\"relational\"];\n"+
			"if header :count \"ge\" :comparator \"i;ascii-numeric\" \"Subject\" \"1\" {\n"+
			"  fileinto %q;\n}\n", folder)
	if err := sieveInject(script, "sender@test.invalid", to, uniqueID(), "relational test", "body"); err != nil {
		return err
	}
	return checkFolder(user, pass, folder)
}

func testSieveDate(user, pass, to string) error {
	clearInbox(user, pass)
	folder := "sieve-test-date"
	if err := createFolder(user, pass, folder); err != nil {
		return fmt.Errorf("pre-create: %w", err)
	}
	script := fmt.Sprintf(
		"require [\"fileinto\",\"date\"];\n"+
			"if date :zone \"+0000\" \"date\" \"year\" \"2026\" {\n"+
			"  fileinto %q;\n}\n", folder)
	if err := sieveInject(script, "sender@test.invalid", to, uniqueID(), "date test", "body"); err != nil {
		return err
	}
	return checkFolder(user, pass, folder)
}

func testSieveEnotify(user, pass, to string) error {
	clearInbox(user, pass)
	token := fmt.Sprintf("XNOTIFY%d", time.Now().UnixNano())
	script := fmt.Sprintf(
		"require \"enotify\";\n"+
			"notify \"mailto:%s?subject=%s\";\n"+
			"keep;\n", to, token)
	// Keep original in INBOX too; inject with real From so notify fires.
	if err := sieveInject(script, "external@other.test", to, uniqueID(), "enotify trigger", "body"); err != nil {
		return err
	}
	// Clean the original trigger message from INBOX.
	cleanInboxBySubject(user, pass, "enotify trigger") //nolint:errcheck

	time.Sleep(10 * time.Second) // allow MX→lmtp→notify→relay-fwd→lmtp delivery chain
	n, err := cleanInboxBySubject(user, pass, token)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no notification email with subject token %q found in INBOX", token)
	}
	return nil
}

// ── main sieve check ───────────────────────────────────────────────────────

func testSieveDebugLog(user, pass, to string) error {
	clearInbox(user, pass)
	uidnext := inboxUIDNext(user, pass)
	script := `require ["variables","envelope","vnd.yarilo.debug"];` + "\n" +
		`if envelope :matches "to" "*" { set "to" "${1}"; }` + "\n" +
		`debug_log "smoke: delivering to ${to}";` + "\n" +
		"keep;\n"
	if err := sieveInject(script, "sender@test.invalid", to, uniqueID(), "debug_log test", "debug_log test body"); err != nil {
		return err
	}
	return inboxWaitByUID(user, pass, uidnext, "debug_log: message was not delivered to INBOX")
}

func testSieveEnvironment(user, pass, to string) error {
	clearInbox(user, pass)
	folder := "sieve-test-environment"
	if err := createFolder(user, pass, folder); err != nil {
		return fmt.Errorf("pre-create: %w", err)
	}
	// Test environment :is with vnd.yarilo.username item AND
	// the env. variable namespace (${env.vnd.yarilo.default_mailbox}).
	// On match: set folder name via env. variable then override to the
	// test folder so the assertion is folder-based (fast, avoids INBOX search).
	script := `require ["environment","variables","fileinto","mailbox","vnd.yarilo.environment"];` + "\n" +
		`set "dest" "Junk";` + "\n" +
		`if environment :is "vnd.yarilo.username" "` + to + `" {` + "\n" +
		`  set "dest" "` + folder + `";` + "\n" +
		`}` + "\n" +
		`fileinto :create "${dest}";` + "\n"
	if err := sieveInject(script, "sender@test.invalid", to, uniqueID(), "environment test", "body"); err != nil {
		return err
	}
	return checkFolder(user, pass, folder)
}

// testSievePipe verifies that vnd.yarilo.pipe is advertised and accepted by
// the Sieve interpreter. The script uses :try so that when no binary exists in
// the sandbox's sieve_pipe_bin_dir the action silently fails and the implicit
// keep fires, delivering the message to INBOX as normal.
func testSievePipe(user, pass, to string) error {
	clearInbox(user, pass)
	id := uniqueID()
	script := `require ["vnd.yarilo.pipe"];` + "\n" +
		`pipe :try "smoketest-noop";` + "\n"
	if err := sieveInject(script, "sender@test.invalid", to, id, "pipe-try test", "pipe test body"); err != nil {
		return err
	}
	// Match the exact injected message by its unique Message-ID rather than a
	// UID range: delivery is asynchronous and the range form is sensitive to
	// unrelated INBOX activity, which made this check flake intermittently.
	return inboxWaitByID(user, pass, id, "pipe :try: message was not delivered to INBOX after failed pipe")
}

// testSieveExecute verifies that vnd.yarilo.execute is accepted by the Sieve
// interpreter. The script uses execute as a test inside if/else so that
// regardless of whether the program exists in the sandbox, the implicit keep
// fires and delivers the message to INBOX.
func testSieveExecute(user, pass, to string) error {
	clearInbox(user, pass)
	uidnext := inboxUIDNext(user, pass)
	script := `require ["vnd.yarilo.execute", "variables"];` + "\n" +
		`set "result" "";` + "\n" +
		`if execute :input "hello" :output "result" "smoketest-noop" {` + "\n" +
		`  keep;` + "\n" +
		`} else {` + "\n" +
		`  keep;` + "\n" +
		`}` + "\n"
	if err := sieveInject(script, "sender@test.invalid", to, uniqueID(), "execute test", "execute test body"); err != nil {
		return err
	}
	return inboxWaitByUID(user, pass, uidnext, "execute test: message was not delivered to INBOX")
}

// testSieveFilter verifies that vnd.yarilo.filter is accepted by the Sieve
// interpreter. The script uses filter as a test inside an if/else so that
// regardless of whether the program exists in the sandbox, the implicit keep
// fires and delivers the message to INBOX.
func testSieveFilter(user, pass, to string) error {
	clearInbox(user, pass)
	uidnext := inboxUIDNext(user, pass)
	script := `require ["vnd.yarilo.filter"];` + "\n" +
		`if filter "smoketest-noop" {` + "\n" +
		`  keep;` + "\n" +
		`} else {` + "\n" +
		`  keep;` + "\n" +
		`}` + "\n"
	if err := sieveInject(script, "sender@test.invalid", to, uniqueID(), "filter test", "filter test body"); err != nil {
		return err
	}
	return inboxWaitByUID(user, pass, uidnext, "filter test: message was not delivered to INBOX")
}

// inboxWaitByUID polls INBOX for any message with UID >= uidnext for up to
// 30 seconds. Uses UID range search (needsBody=false) so no per-message NFS
// reads are required — only the in-memory fileindex is consulted.
// inboxWaitByID polls INBOX until a message carrying the given Message-ID
// appears, then deletes it. Unlike inboxWaitByUID it identifies the exact
// injected message, so it is immune to unrelated deliveries racing into INBOX.
func inboxWaitByID(user, pass, id, failMsg string) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		c, err := imapDial()
		if err != nil {
			return err
		}
		if err := c.login(user, pass); err != nil {
			c.close()
			return err
		}
		if _, err := c.selectFolder("INBOX"); err != nil {
			c.close()
			return fmt.Errorf("SELECT INBOX: %w", err)
		}
		uids, err := c.uidSearch(fmt.Sprintf("HEADER Message-ID \"<%s>\"", id))
		if err != nil {
			c.close()
			return fmt.Errorf("UID SEARCH: %w", err)
		}
		if len(uids) > 0 {
			c.deleteUIDs(uids) //nolint:errcheck
			c.close()
			return nil
		}
		c.close()
		if time.Now().After(deadline) {
			return fmt.Errorf("%s", failMsg)
		}
		time.Sleep(1 * time.Second)
	}
}

func inboxWaitByUID(user, pass string, uidnext int, failMsg string) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		c, err := imapDial()
		if err != nil {
			return err
		}
		if err := c.login(user, pass); err != nil {
			c.close()
			return err
		}
		if _, err := c.selectFolder("INBOX"); err != nil {
			c.close()
			return fmt.Errorf("SELECT INBOX: %w", err)
		}
		uids, err := c.uidSearch(fmt.Sprintf("UID %d:*", uidnext))
		if err != nil {
			c.close()
			return fmt.Errorf("UID SEARCH: %w", err)
		}
		if len(uids) > 0 {
			c.deleteUIDs(uids) //nolint:errcheck
			c.close()
			return nil
		}
		c.close()
		if time.Now().After(deadline) {
			return fmt.Errorf("%s", failMsg)
		}
		time.Sleep(1 * time.Second)
	}
}

// inboxUIDNext returns the UIDNEXT value for INBOX, or 1 on any error.
func inboxUIDNext(user, pass string) int {
	c, err := imapDial()
	if err != nil {
		return 1
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return 1
	}
	lines, err := c.cmd(`SELECT "INBOX"`)
	if err != nil {
		return 1
	}
	for _, l := range lines {
		var n int
		if _, e := fmt.Sscanf(l, "* OK [UIDNEXT %d]", &n); e == nil {
			return n
		}
	}
	return 1
}

func checkSieve() error {
	if *flagManageSieveUser == "" {
		return fmt.Errorf("-managesieve-user is required")
	}
	user := *flagManageSieveUser
	pass := *flagManageSievePass
	to := *flagManageSieveUser

	defer msieveDeactivateAndDelete(sieveScriptNameConst)

	type sieveTest struct {
		name string
		fn   func(user, pass, to string) error
	}
	tests := []sieveTest{
		{"fileinto", testSieveFileinto},
		{"fileinto+mailbox:create", testSieveMailbox},
		{"imap4flags", testSieveImap4flags},
		{"body", testSieveBody},
		{"envelope", testSieveEnvelope},
		{"variables", testSieveVariables},
		{"reject", testSieveReject},
		{"ereject", testSieveEreject},
		{"duplicate", testSieveDuplicate},
		{"relational", testSieveRelational},
		{"date", testSieveDate},
		{"vnd.yarilo.debug", testSieveDebugLog},
		{"vnd.yarilo.environment", testSieveEnvironment},
		{"vnd.yarilo.pipe", testSievePipe},
		{"vnd.yarilo.filter", testSieveFilter},
		{"vnd.yarilo.execute", testSieveExecute},
		{"enotify", testSieveEnotify},
		{"foreverypart+mime", testSieveForeverypart},
		{"max_actions", testSieveMaxActions},
		{"spamtest", testSieveSpamtest},
		{"mailboxid", testSieveMailboxID},
		{"metadata", testSieveMetadata},
		{"report", testSieveReport},
		{"imap objectid", testIMAPObjectID},
	}

	slog.Info("sieve: start", "total", len(tests))
	var errs []string
	for i, t := range tests {
		slog.Info("sieve: run", "n", i+1, "total", len(tests), "test", t.name)
		if err := t.fn(user, pass, to); err != nil {
			slog.Error("sieve: FAIL", "n", i+1, "total", len(tests), "test", t.name, "err", err)
			errs = append(errs, fmt.Sprintf("  %s: %v", t.name, err))
		} else {
			slog.Info("sieve: OK", "n", i+1, "total", len(tests), "test", t.name)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("plugin failures:\n%s", strings.Join(errs, "\n"))
	}
	return nil
}

// lmtpSendRaw injects a complete raw message (headers + body) via LMTP, for
// tests that need custom headers (Content-Type multipart, X-Spam-Score, ...).
func lmtpSendRaw(from, to, raw string) error {
	addr := net.JoinHostPort(smtpHost(), *flagSieveSMTPPort)
	conn, err := net.DialTimeout("tcp", addr, *flagTimeout)
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(*flagTimeout)) //nolint:errcheck
	r := bufio.NewReader(conn)
	readResp := func() (string, error) {
		var last string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return "", err
			}
			last = strings.TrimRight(line, "\r\n")
			if len(line) < 4 || line[3] != '-' {
				return last, nil
			}
		}
	}
	cmd := func(c string) (string, error) { fmt.Fprintf(conn, "%s\r\n", c); return readResp() }
	if _, err := readResp(); err != nil {
		return fmt.Errorf("greeting: %w", err)
	}
	if resp, err := cmd("EHLO smoketest"); err != nil || !strings.HasPrefix(resp, "250") {
		return fmt.Errorf("EHLO: %s %v", resp, err)
	}
	if resp, err := cmd("MAIL FROM:<" + from + ">"); err != nil || !strings.HasPrefix(resp, "250") {
		return fmt.Errorf("MAIL FROM: %s %v", resp, err)
	}
	if resp, err := cmd("RCPT TO:<" + to + ">"); err != nil || !strings.HasPrefix(resp, "250") {
		return fmt.Errorf("RCPT TO: %s %v", resp, err)
	}
	if resp, err := cmd("DATA"); err != nil || !strings.HasPrefix(resp, "354") {
		return fmt.Errorf("DATA: %s %v", resp, err)
	}
	fmt.Fprintf(conn, "%s\r\n.\r\n", raw)
	if _, err := readResp(); err != nil {
		return fmt.Errorf("end-of-data: %w", err)
	}
	cmd("QUIT") //nolint:errcheck
	return nil
}

func joined(lines []string) string { return strings.Join(lines, "\n") }

// testIMAPObjectID verifies RFC 8474: OBJECTID capability, MAILBOXID in SELECT,
// and EMAILID in FETCH.
func testIMAPObjectID(user, pass, to string) error {
	// Deterministic delivery: empty INBOX + a plain keep script so the message
	// lands in INBOX regardless of any leftover active script.
	clearInbox(user, pass)
	if err := msieveSetActive("keep;\n"); err != nil {
		return fmt.Errorf("msieve keep: %w", err)
	}

	c, err := imapDial()
	if err != nil {
		return err
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return err
	}
	caps, err := c.cmd("CAPABILITY")
	if err != nil {
		return err
	}
	if !strings.Contains(joined(caps), "OBJECTID") {
		return fmt.Errorf("CAPABILITY missing OBJECTID: %s", joined(caps))
	}
	sel, err := c.cmd(`SELECT "INBOX"`)
	if err != nil {
		return err
	}
	if !strings.Contains(joined(sel), "MAILBOXID (") {
		return fmt.Errorf("SELECT INBOX missing MAILBOXID: %s", joined(sel))
	}
	id := fmt.Sprintf("objectid-%d@test", time.Now().UnixNano())
	if err := lmtpSend(id, "s@test.invalid", to, "objectid", "body"); err != nil {
		return fmt.Errorf("inject: %w", err)
	}
	// Delivery + indexing is async; re-SELECT and search a few times.
	var uids []string
	for i := 0; i < 10; i++ {
		if _, err := c.cmd(`SELECT "INBOX"`); err != nil {
			return err
		}
		uids, _ = c.uidSearch(fmt.Sprintf("HEADER Message-ID \"<%s>\"", id))
		if len(uids) > 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(uids) == 0 {
		return fmt.Errorf("delivered message not found")
	}
	f, err := c.cmd(fmt.Sprintf("UID FETCH %s (EMAILID)", uids[0]))
	if err != nil {
		return err
	}
	if !strings.Contains(joined(f), "EMAILID (") {
		return fmt.Errorf("FETCH missing EMAILID: %s", joined(f))
	}
	c.deleteUIDs(uids) //nolint:errcheck
	return nil
}

// testSieveForeverypart verifies RFC 5703 foreverypart + mime: a multipart
// message's HTML part routes to a folder.
func testSieveForeverypart(user, pass, to string) error {
	clearInbox(user, pass)
	folder := "sieve-test-mime"
	if err := createFolder(user, pass, folder); err != nil {
		return fmt.Errorf("pre-create: %w", err)
	}
	script := "require [\"foreverypart\",\"mime\",\"fileinto\"];\n" +
		"foreverypart { if header :mime :subtype \"Content-Type\" \"html\" { fileinto \"" + folder + "\"; } }\n"
	if err := msieveSetActive(script); err != nil {
		return fmt.Errorf("msieve: %w", err)
	}
	id := fmt.Sprintf("mime-%d@test", time.Now().UnixNano())
	raw := "Message-ID: <" + id + ">\r\nFrom: <s@test.invalid>\r\nTo: <" + to + ">\r\nSubject: mime\r\n" +
		"Content-Type: multipart/alternative; boundary=bb\r\n\r\n" +
		"--bb\r\nContent-Type: text/plain\r\n\r\nplain\r\n" +
		"--bb\r\nContent-Type: text/html\r\n\r\n<p>h</p>\r\n--bb--\r\n"
	if err := lmtpSendRaw("s@test.invalid", to, raw); err != nil {
		return fmt.Errorf("inject: %w", err)
	}
	return checkFolder(user, pass, folder)
}

// testSieveMaxActions verifies sieve_max_actions: a script exceeding the cap
// aborts and falls back to implicit keep (INBOX).
func testSieveMaxActions(user, pass, to string) error {
	clearInbox(user, pass)
	var b strings.Builder
	b.WriteString("require [\"fileinto\",\"mailbox\"];\n")
	for i := 0; i < 40; i++ { // > default cap 32
		fmt.Fprintf(&b, "fileinto :create \"MA%d\";\n", i)
	}
	if err := msieveSetActive(b.String()); err != nil {
		return fmt.Errorf("msieve: %w", err)
	}
	id := fmt.Sprintf("maxact-%d@test", time.Now().UnixNano())
	if err := lmtpSend(id, "s@test.invalid", to, "maxactions", "body"); err != nil {
		return fmt.Errorf("inject: %w", err)
	}
	return checkFolder(user, pass, "INBOX") // implicit keep
}

// testSieveMailboxID verifies RFC 9042: fileinto :mailboxid delivers to the
// folder carrying the given MAILBOXID rather than the positional fallback.
func testSieveMailboxID(user, pass, to string) error {
	clearInbox(user, pass)
	folder := "sieve-test-mboxid"
	if err := createFolder(user, pass, folder); err != nil {
		return fmt.Errorf("pre-create: %w", err)
	}

	// Read the target folder's server-assigned MAILBOXID from SELECT.
	c, err := imapDial()
	if err != nil {
		return err
	}
	if err := c.login(user, pass); err != nil {
		c.close()
		return err
	}
	sel, err := c.cmd(fmt.Sprintf("SELECT %q", folder))
	c.close()
	if err != nil {
		return fmt.Errorf("SELECT %q: %w", folder, err)
	}
	mboxID := extractMailboxID(joined(sel))
	if mboxID == "" {
		return fmt.Errorf("SELECT %q missing MAILBOXID: %s", folder, joined(sel))
	}

	// :mailboxid resolves to `folder`; the "Fallback" positional is a trap —
	// if resolution failed the message would land there instead.
	script := "require [\"fileinto\",\"mailboxid\"];\n" +
		"fileinto :mailboxid \"" + mboxID + "\" \"Fallback\";\n"
	if err := msieveSetActive(script); err != nil {
		return fmt.Errorf("msieve: %w", err)
	}
	id := fmt.Sprintf("mboxid-%d@test", time.Now().UnixNano())
	if err := lmtpSend(id, "s@test.invalid", to, "mailboxid", "body"); err != nil {
		return fmt.Errorf("inject: %w", err)
	}
	return checkFolder(user, pass, folder)
}

// extractMailboxID pulls the objectid out of a `MAILBOXID (<id>)` response code.
func extractMailboxID(s string) string {
	const marker = "MAILBOXID ("
	i := strings.Index(s, marker)
	if i < 0 {
		return ""
	}
	rest := s[i+len(marker):]
	j := strings.IndexByte(rest, ')')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// testSieveMetadata verifies RFC 5490 §4 mboxmetadata + servermetadata: a Sieve
// script routes on IMAP METADATA annotations set via SETMETADATA. Sets a
// mailbox-scoped annotation on INBOX and a server-scoped one, then asserts a
// message keying on both lands in the target folder.
func testSieveMetadata(user, pass, to string) error {
	clearInbox(user, pass)
	folder := "sieve-test-meta"
	if err := createFolder(user, pass, folder); err != nil {
		return fmt.Errorf("pre-create: %w", err)
	}

	c, err := imapDial()
	if err != nil {
		return err
	}
	if err := c.login(user, pass); err != nil {
		c.close()
		return err
	}
	if _, err := c.cmd(`SETMETADATA "INBOX" (/private/vnd.yarilo.sievetest "vip")`); err != nil {
		c.close()
		return fmt.Errorf("SETMETADATA mailbox: %w", err)
	}
	if _, err := c.cmd(`SETMETADATA "" (/shared/vnd.yarilo.sievetest "on")`); err != nil {
		c.close()
		return fmt.Errorf("SETMETADATA server: %w", err)
	}
	c.close()

	script := "require [\"mboxmetadata\",\"servermetadata\",\"fileinto\"];\n" +
		"if allof(metadata \"INBOX\" \"/private/vnd.yarilo.sievetest\" \"vip\", " +
		"servermetadata \"/shared/vnd.yarilo.sievetest\" \"on\") { fileinto \"" + folder + "\"; }\n"
	if err := msieveSetActive(script); err != nil {
		return fmt.Errorf("msieve: %w", err)
	}
	id := fmt.Sprintf("meta-%d@test", time.Now().UnixNano())
	if err := lmtpSend(id, "s@test.invalid", to, "metadata", "body"); err != nil {
		return fmt.Errorf("inject: %w", err)
	}
	return checkFolder(user, pass, folder)
}

// testSieveReport verifies vnd.yarilo.report (RFC 5965 ARF) end to end: the
// script reports the delivered trigger back to the recipient, so the ARF report
// routes through submission -> LMTP into INBOX, and we confirm its multipart/report
// structure. The report is guarded on the trigger's subject so the delivered
// report (subject "abuse report") does not re-trigger itself into a mail loop.
func testSieveReport(user, pass, to string) error {
	clearInbox(user, pass)
	script := "require [\"vnd.yarilo.report\"];\n" +
		"if header :contains \"subject\" \"report-trigger\" {\n" +
		"  report \"abuse\" \"smoke abuse report\" \"" + to + "\";\n" +
		"}\n"
	if err := msieveSetActive(script); err != nil {
		return fmt.Errorf("msieve: %w", err)
	}
	id := fmt.Sprintf("report-%d@test", time.Now().UnixNano())
	if err := lmtpSend(id, "s@test.invalid", to, "report-trigger", "body"); err != nil {
		return fmt.Errorf("inject: %w", err)
	}

	c, err := imapDial()
	if err != nil {
		return err
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return err
	}
	// The report is submitted asynchronously and routed back via LMTP. Poll until
	// a delivered message matches the feedback-report content type (which excludes
	// the trigger) AND fetches as a valid multipart/report.
	var found bool
	for i := 0; i < 30 && !found; i++ {
		c.conn.SetDeadline(time.Now().Add(*flagTimeout)) //nolint:errcheck
		if _, err := c.cmd(`SELECT "INBOX"`); err != nil {
			return err
		}
		uids, _ := c.uidSearch(`HEADER "Content-Type" "feedback-report"`)
		for _, uid := range uids {
			hdr, err := c.cmd(fmt.Sprintf("UID FETCH %s (BODY.PEEK[HEADER])", uid))
			if err != nil {
				continue
			}
			raw := joined(hdr)
			if strings.Contains(raw, "multipart/report") && strings.Contains(raw, "report-type=feedback-report") {
				found = true
				break
			}
		}
		if !found {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if !found {
		return fmt.Errorf("no valid ARF report delivered back to INBOX within timeout")
	}
	all, _ := c.uidSearch("ALL")
	c.deleteUIDs(all) //nolint:errcheck
	return nil
}

// testSieveSpamtest verifies RFC 5235 spamtest against the configured
// status header (requires sieve_spamtest_status_header set).
func testSieveSpamtest(user, pass, to string) error {
	clearInbox(user, pass)
	folder := "sieve-test-spam"
	if err := createFolder(user, pass, folder); err != nil {
		return fmt.Errorf("pre-create: %w", err)
	}
	script := "require [\"spamtest\",\"relational\",\"comparator-i;ascii-numeric\",\"fileinto\"];\n" +
		"if spamtest :value \"ge\" :comparator \"i;ascii-numeric\" \"8\" { fileinto \"" + folder + "\"; }\n"
	if err := msieveSetActive(script); err != nil {
		return fmt.Errorf("msieve: %w", err)
	}
	id := fmt.Sprintf("spam-%d@test", time.Now().UnixNano())
	raw := "Message-ID: <" + id + ">\r\nFrom: <s@test.invalid>\r\nTo: <" + to + ">\r\nSubject: spam\r\n" +
		"X-Spam-Score: 9\r\n\r\nbuy now\r\n"
	if err := lmtpSendRaw("s@test.invalid", to, raw); err != nil {
		return fmt.Errorf("inject: %w", err)
	}
	return checkFolder(user, pass, folder)
}
