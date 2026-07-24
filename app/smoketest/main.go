// smoketest verifies a live yarilo deployment.
// Usage: smoketest -host mail.example.com -telemetry http://...:8080
//
// Exit 0 = all checks passed.
// Exit 1 = one or more checks failed (prints failures to stderr).
// IMAP conformance is covered by imaptest (see smoke.yml).
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

var (
	flagHost            = flag.String("host", "localhost", "yarilo hostname")
	flagIMAPHost        = flag.String("imap-host", "", "IMAP hostname for sieve verify step (defaults to -host)")
	flagSMTPHost        = flag.String("smtp-host", "", "SMTP hostname for sieve mail injection (defaults to -host)")
	flagIMAPSPort       = flag.String("imap-port", "993", "IMAPS port (used by sieve verify step)")
	flagPOP3SPort       = flag.String("pop3s-port", "995", "POP3S port")
	flagSMTPMXPort      = flag.String("smtp-mx-port", "25", "SMTP MX port")
	flagSMTPSubPort     = flag.String("smtp-sub-port", "587", "SMTP submission port")
	flagLMTPLoginPort   = flag.String("lmtp-login-port", "24", "yarilo-lmtp-login port")
	flagManageSievePort = flag.String("managesieve-port", "4190", "ManageSieve port")
	flagManageSieveUser = flag.String("managesieve-user", "", "ManageSieve PLAIN auth username")
	flagManageSievePass = flag.String("managesieve-pass", "", "ManageSieve PLAIN auth password")
	flagTelemetry       = flag.String("telemetry", "http://localhost:8080", "telemetry base URL")
	flagTimeout         = flag.Duration("timeout", 10*time.Second, "per-check timeout")
	flagInsecure        = flag.Bool("insecure", false, "skip TLS certificate verification")
	flagSMTPMX          = flag.Bool("smtp-mx", false, "check SMTP MX EHLO (port -smtp-mx-port)")
	flagSMTPSub         = flag.Bool("smtp-sub", false, "check SMTP submission EHLO+STARTTLS (port -smtp-sub-port)")
	flagProxyProtocol   = flag.Bool("proxy-protocol", false, "send HAProxy PROXY header before SMTP banner")
	flagXClient         = flag.Bool("xclient", false, "check that MX port advertises XCLIENT in EHLO")
	flagPOP3S           = flag.Bool("pop3s", false, "check POP3S greeting and CAPA")
	flagLMTPLogin       = flag.Bool("lmtp-login", false, "check yarilo-lmtp-login LHLO greeting (port -lmtp-login-port)")
	flagManageSieve     = flag.Bool("managesieve", false, "check ManageSieve auth + script CRUD (port -managesieve-port)")
	flagSieve           = flag.Bool("sieve", false, "check Sieve plugin execution via SMTP injection + IMAP verify")
	flagSieveSMTPPort   = flag.String("sieve-smtp-port", "25", "SMTP MX port for Sieve mail injection")

	flagPasswdFileUser = flag.String("passwd-file-user", "", "IMAP username backed by the passwd-file passdb (enables the check)")
	flagPasswdFilePass = flag.String("passwd-file-pass", "", "password for -passwd-file-user")
	flagStaticUser     = flag.String("static-user", "", "IMAP username backed by the static passdb (enables the check)")
	flagStaticPass     = flag.String("static-pass", "", "password for -static-user")

	flagQuotaUser     = flag.String("quota-user", "", "IMAP username for the QUOTA extension check (enables it)")
	flagQuotaPass     = flag.String("quota-pass", "", "password for -quota-user")
	flagQuotaOverUser = flag.String("quota-over-user", "", "IMAP username provisioned over quota, for the enforcement check (enables it)")
	flagQuotaOverPass = flag.String("quota-over-pass", "", "password for -quota-over-user")
	flagACLUser       = flag.String("acl-user", "", "IMAP username for the ACL extension check (enables it)")
	flagACLPass       = flag.String("acl-pass", "", "password for -acl-user")

	flagFTSUser = flag.String("fts-user", "", "IMAP username for the full-text search check (enables it)")
	flagFTSPass = flag.String("fts-pass", "", "password for -fts-user")

	flagDirectorAPI      = flag.String("director-api", "", "director admin API base URL, e.g. http://yarilo-director-api:9103 (enables the check, #755)")
	flagDirectorAPIToken = flag.String("director-api-token", "", "director admin API bearer token (defaults to env DIRECTOR_API_TOKEN / YARILO_ADMIN_TOKEN)")
)

type result struct {
	name string
	err  error
}

func imapHost() string {
	if *flagIMAPHost != "" {
		return *flagIMAPHost
	}
	return *flagHost
}

func smtpHost() string {
	if *flagSMTPHost != "" {
		return *flagSMTPHost
	}
	return *flagHost
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	flag.Parse()

	checks := []struct {
		name string
		fn   func() error
	}{}
	if *flagTelemetry != "" {
		checks = append(checks,
			struct {
				name string
				fn   func() error
			}{"telemetry /healthz", checkHealth},
			struct {
				name string
				fn   func() error
			}{"telemetry /readyz", checkReady},
		)
	}

	if *flagSMTPMX {
		checks = append(checks, struct {
			name string
			fn   func() error
		}{"smtp MX EHLO", checkSMTPMX})
	}
	if *flagSMTPSub {
		checks = append(checks, struct {
			name string
			fn   func() error
		}{"smtp submission EHLO+STARTTLS", checkSMTPSubmission})
	}
	if *flagProxyProtocol {
		checks = append(checks, struct {
			name string
			fn   func() error
		}{"smtp MX PROXY protocol", checkSMTPProxyProtocol})
	}
	if *flagXClient {
		checks = append(checks, struct {
			name string
			fn   func() error
		}{"smtp MX XCLIENT cap", checkSMTPXClient})
	}
	if *flagPOP3S {
		checks = append(checks, struct {
			name string
			fn   func() error
		}{"pop3s CAPA", checkPOP3S})
	}
	if *flagLMTPLogin {
		checks = append(checks, struct {
			name string
			fn   func() error
		}{"lmtp-login LHLO", checkLMTPLogin})
	}
	if *flagManageSieve {
		checks = append(checks, struct {
			name string
			fn   func() error
		}{"managesieve auth+CRUD", checkManageSieve})
	}
	if *flagSieve {
		checks = append(checks, struct {
			name string
			fn   func() error
		}{"sieve plugins", checkSieve})
	}
	if *flagPasswdFileUser != "" {
		checks = append(checks, struct {
			name string
			fn   func() error
		}{"imap login (passwd-file passdb)", func() error {
			return checkIMAPLogin(*flagPasswdFileUser, *flagPasswdFilePass)
		}})
	}
	if *flagStaticUser != "" {
		checks = append(checks, struct {
			name string
			fn   func() error
		}{"imap login (static passdb)", func() error {
			return checkIMAPLogin(*flagStaticUser, *flagStaticPass)
		}})
	}
	if *flagQuotaUser != "" {
		checks = append(checks, struct {
			name string
			fn   func() error
		}{"imap QUOTA (GETQUOTA)", func() error {
			return checkQuota(*flagQuotaUser, *flagQuotaPass)
		}})
	}
	if *flagQuotaOverUser != "" {
		checks = append(checks, struct {
			name string
			fn   func() error
		}{"imap QUOTA enforcement (OVERQUOTA)", func() error {
			return checkQuotaOver(*flagQuotaOverUser, *flagQuotaOverPass)
		}})
	}
	if *flagACLUser != "" {
		checks = append(checks, struct {
			name string
			fn   func() error
		}{"imap ACL (MYRIGHTS + SETACL round-trip)", func() error {
			return checkACL(*flagACLUser, *flagACLPass)
		}})
	}
	if *flagFTSUser != "" {
		checks = append(checks, struct {
			name string
			fn   func() error
		}{"imap FTS (SEARCH BODY/TEXT/HEADER/FROM)", func() error {
			return checkFTS(*flagFTSUser, *flagFTSPass)
		}})
	}
	if *flagDirectorAPI != "" {
		checks = append(checks, struct {
			name string
			fn   func() error
		}{"director admin API status (authenticated)", checkDirectorAPI})
	}

	slog.Info("smoke: start", "total", len(checks))
	var failures []result
	for i, c := range checks {
		slog.Info("smoke: run", "n", i+1, "total", len(checks), "check", c.name)
		if err := c.fn(); err != nil {
			slog.Error("smoke: FAIL", "n", i+1, "total", len(checks), "check", c.name, "err", err)
			failures = append(failures, result{c.name, err})
		} else {
			slog.Info("smoke: OK", "n", i+1, "total", len(checks), "check", c.name)
		}
	}

	if len(failures) > 0 {
		fmt.Fprintf(os.Stderr, "\n%d smoke check(s) failed:\n", len(failures))
		for _, f := range failures {
			fmt.Fprintf(os.Stderr, "  - %s: %v\n", f.name, f.err)
		}
		os.Exit(1)
	}
}

// ---- IMAP login (passdb drivers) -----------------------------------------

// checkIMAPLogin proves an IMAP LOGIN succeeds for a user served by a specific
// passdb driver (passwd-file / static). It dials IMAPS, authenticates, and
// selects INBOX to confirm the userdb resolved a mailbox for the account.
func checkIMAPLogin(user, pass string) error {
	c, err := imapDial()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return fmt.Errorf("login %q: %w", user, err)
	}
	if _, err := c.selectFolder("INBOX"); err != nil {
		return fmt.Errorf("select INBOX: %w", err)
	}
	return nil
}

// ---- telemetry -----------------------------------------------------------

func checkHealth() error { return httpGet(*flagTelemetry + "/healthz") }
func checkReady() error  { return httpGet(*flagTelemetry + "/readyz") }

func httpGet(url string) error {
	c := &http.Client{Timeout: *flagTimeout}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	return nil
}

// checkDirectorAPI verifies the director admin API authenticates a bearer
// token (#755): the whole class of bug was `yarilo-admin director status`
// returning 403 from every pod because the token was never plumbed. This
// hits GET /api/director/ring (the member/peer list) with the token and
// asserts a 200 with a member list — a 403 is the exact regression to
// catch, so it is reported distinctly from any other non-200. (Uses /ring
// rather than /status because status is now backends-only — the peer list
// moved to its dedicated endpoint.)
func checkDirectorAPI() error {
	token := *flagDirectorAPIToken
	if token == "" {
		token = os.Getenv("DIRECTOR_API_TOKEN")
	}
	if token == "" {
		token = os.Getenv("YARILO_ADMIN_TOKEN")
	}
	url := strings.TrimRight(*flagDirectorAPI, "/") + "/api/director/ring"
	c := &http.Client{Timeout: *flagTimeout}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("director API rejected the token (HTTP %d) — the #755 plumbing is broken: %s", resp.StatusCode, body)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("peers")) {
		return fmt.Errorf("director API status returned 200 but no member list: %s", body)
	}
	return nil
}

// ---- POP3S (port 995) ----------------------------------------------------

func checkPOP3S() error {
	addr := net.JoinHostPort(*flagHost, *flagPOP3SPort)
	dialer := &net.Dialer{Timeout: *flagTimeout}
	tlsCfg := &tls.Config{
		ServerName:         *flagHost,
		InsecureSkipVerify: *flagInsecure, //nolint:gosec
	}
	conn, err := tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(*flagTimeout)) //nolint:errcheck

	greeting, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("read greeting: %w", err)
	}
	if !strings.HasPrefix(greeting, "+OK") {
		return fmt.Errorf("unexpected POP3 greeting: %q", greeting)
	}

	fmt.Fprintf(conn, "CAPA\r\n")
	resp, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("CAPA response: %w", err)
	}
	if !strings.HasPrefix(resp, "+OK") {
		return fmt.Errorf("CAPA failed: %q", resp)
	}
	foundUSER := false
	for {
		line, err := readLine(conn)
		if err != nil {
			return fmt.Errorf("CAPA read: %w", err)
		}
		if line == "." {
			break
		}
		if strings.HasPrefix(line, "USER") {
			foundUSER = true
		}
	}
	if !foundUSER {
		return fmt.Errorf("CAPA missing USER capability")
	}

	fmt.Fprintf(conn, "QUIT\r\n")
	return nil
}

// ---- LMTP login (port 24) ------------------------------------------------

// checkLMTPLogin connects to yarilo-lmtp-login, verifies the 220 banner,
// sends LHLO, and expects a 250 multi-line response with at least one known
// LMTP extension before quitting cleanly.
func checkLMTPLogin() error {
	addr := net.JoinHostPort(*flagHost, *flagLMTPLoginPort)
	dialer := &net.Dialer{Timeout: *flagTimeout}
	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(*flagTimeout)) //nolint:errcheck

	banner, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("read banner: %w", err)
	}
	if !strings.HasPrefix(banner, "220") {
		return fmt.Errorf("unexpected banner: %q", banner)
	}

	fmt.Fprintf(conn, "LHLO smoketest\r\n")
	gotOK := false
	for {
		line, err := readLine(conn)
		if err != nil {
			return fmt.Errorf("LHLO read: %w", err)
		}
		if strings.HasPrefix(line, "250-") || strings.HasPrefix(line, "250 ") {
			gotOK = true
			if strings.HasPrefix(line, "250 ") {
				break
			}
			continue
		}
		return fmt.Errorf("LHLO unexpected response: %q", line)
	}
	if !gotOK {
		return fmt.Errorf("LHLO: no 250 response received")
	}

	fmt.Fprintf(conn, "QUIT\r\n")
	return nil
}

// ---- SMTP MX (port 25) ---------------------------------------------------

func checkSMTPMX() error {
	conn, err := smtpDial(net.JoinHostPort(*flagHost, *flagSMTPMXPort), false)
	if err != nil {
		return err
	}
	defer conn.Close()

	caps, err := smtpEHLO(conn)
	if err != nil {
		return err
	}
	_ = caps
	smtpQuit(conn)
	return nil
}

// checkSMTPSubmission verifies the submission port:
//  1. EHLO advertises AUTH PLAIN and STARTTLS.
//  2. Performs the STARTTLS upgrade and sends a second EHLO.
func checkSMTPSubmission() error {
	addr := net.JoinHostPort(*flagHost, *flagSMTPSubPort)
	conn, err := smtpDial(addr, false)
	if err != nil {
		return err
	}
	defer conn.Close()

	caps, err := smtpEHLO(conn)
	if err != nil {
		return err
	}
	if !caps["STARTTLS"] {
		return fmt.Errorf("submission port did not advertise STARTTLS")
	}
	if !caps["AUTH PLAIN"] {
		return fmt.Errorf("submission port did not advertise AUTH PLAIN")
	}

	// Perform STARTTLS upgrade.
	fmt.Fprintf(conn, "STARTTLS\r\n")
	line, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("STARTTLS: read response: %w", err)
	}
	if !strings.HasPrefix(line, "220") {
		return fmt.Errorf("STARTTLS: unexpected response %q", line)
	}
	tlsCfg := &tls.Config{
		ServerName:         *flagHost,
		InsecureSkipVerify: *flagInsecure, //nolint:gosec
	}
	tlsConn := tls.Client(conn, tlsCfg)
	if err := tlsConn.Handshake(); err != nil {
		return fmt.Errorf("STARTTLS TLS handshake: %w", err)
	}

	// Second EHLO after upgrade must still advertise AUTH PLAIN.
	caps2, err := smtpEHLO(tlsConn)
	if err != nil {
		return fmt.Errorf("post-STARTTLS EHLO: %w", err)
	}
	if !caps2["AUTH PLAIN"] {
		return fmt.Errorf("post-STARTTLS EHLO missing AUTH PLAIN")
	}
	smtpQuit(tlsConn)
	return nil
}

// checkSMTPProxyProtocol sends a HAProxy PROXY header before the SMTP banner
// and verifies the server responds with 220.
// Only run when -proxy-protocol flag is set (requires proxy_protocol: true in config).
func checkSMTPProxyProtocol() error {
	addr := net.JoinHostPort(*flagHost, *flagSMTPMXPort)
	dialer := &net.Dialer{Timeout: *flagTimeout}
	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(*flagTimeout)) //nolint:errcheck

	// Send HAProxy PROXY header with a fake source IP.
	fmt.Fprintf(conn, "PROXY TCP4 203.0.113.1 %s 12345 25\r\n", *flagHost)

	// Expect normal SMTP banner.
	banner, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("PROXY: read banner: %w", err)
	}
	if !strings.HasPrefix(banner, "220") {
		return fmt.Errorf("PROXY: unexpected banner %q", banner)
	}
	return nil
}

// checkSMTPXClient connects to MX and verifies EHLO advertises XCLIENT.
// Only run when -xclient flag is set (requires xclient: true in config).
func checkSMTPXClient() error {
	conn, err := smtpDial(net.JoinHostPort(*flagHost, *flagSMTPMXPort), false)
	if err != nil {
		return err
	}
	defer conn.Close()

	caps, err := smtpEHLO(conn)
	if err != nil {
		return err
	}
	if !caps["XCLIENT"] {
		return fmt.Errorf("MX port did not advertise XCLIENT in EHLO")
	}
	smtpQuit(conn)
	return nil
}

// ---- SMTP helpers --------------------------------------------------------

// smtpDial opens a raw TCP connection and reads the 220 banner.
func smtpDial(addr string, _ bool) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: *flagTimeout}
	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", addr, err)
	}
	conn.SetDeadline(time.Now().Add(*flagTimeout)) //nolint:errcheck

	banner, err := readLine(conn)
	if err != nil {
		conn.Close() //nolint:errcheck
		return nil, fmt.Errorf("read banner: %w", err)
	}
	if !strings.HasPrefix(banner, "220") {
		conn.Close() //nolint:errcheck
		return nil, fmt.Errorf("unexpected banner: %q", banner)
	}
	return conn, nil
}

// smtpEHLO sends EHLO and returns the set of advertised capabilities.
// Keys are normalised: "AUTH PLAIN", "STARTTLS", "XCLIENT", "PIPELINING", …
func smtpEHLO(conn net.Conn) (map[string]bool, error) {
	fmt.Fprintf(conn, "EHLO smoketest\r\n")
	caps := make(map[string]bool)
	for {
		line, err := readLine(conn)
		if err != nil {
			return nil, fmt.Errorf("EHLO read: %w", err)
		}
		// strip "250-" or "250 " prefix
		cap := ""
		if strings.HasPrefix(line, "250-") {
			cap = line[4:]
		} else if strings.HasPrefix(line, "250 ") {
			cap = line[4:]
			caps[cap] = true
			break
		} else {
			return nil, fmt.Errorf("EHLO unexpected: %q", line)
		}
		caps[cap] = true
	}
	return caps, nil
}

func smtpQuit(conn net.Conn) {
	fmt.Fprintf(conn, "QUIT\r\n")
}

// ---- ManageSieve (RFC 5804, port 4190) -----------------------------------

// checkManageSieve connects to the ManageSieve login proxy, authenticates via
// PLAIN, then runs a full script CRUD cycle: LISTSCRIPTS → PUTSCRIPT →
// GETSCRIPT → SETACTIVE → deactivate → DELETESCRIPT → LOGOUT.
func checkManageSieve() error {
	if *flagManageSieveUser == "" {
		return fmt.Errorf("-managesieve-user is required for ManageSieve check")
	}

	conn, err := msieveDial()
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()

	if err := msieveAuth(conn, *flagManageSieveUser, *flagManageSievePass); err != nil {
		return err
	}

	// LISTSCRIPTS
	fmt.Fprintf(conn, "LISTSCRIPTS\r\n")
	if err := msieveDrainUntilOK(conn); err != nil {
		return fmt.Errorf("LISTSCRIPTS: %w", err)
	}

	// PUTSCRIPT — non-synchronising literal ({N+}) so no continuation needed.
	const scriptName = "smoke-check.sieve"
	const scriptBody = "keep;\n"
	fmt.Fprintf(conn, "PUTSCRIPT %q {%d+}\r\n%s", scriptName, len(scriptBody), scriptBody)
	line, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("PUTSCRIPT response: %w", err)
	}
	if !strings.HasPrefix(line, "OK") {
		return fmt.Errorf("PUTSCRIPT failed: %q", line)
	}

	// GETSCRIPT — server responds with {N}\r\n<data>\r\nOK ...
	fmt.Fprintf(conn, "GETSCRIPT %q\r\n", scriptName)
	litHdr, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("GETSCRIPT literal header: %w", err)
	}
	if !strings.HasPrefix(litHdr, "{") {
		return fmt.Errorf("GETSCRIPT unexpected: %q", litHdr)
	}
	sizeStr := strings.TrimSuffix(strings.TrimPrefix(litHdr, "{"), "}")
	size, err := strconv.Atoi(strings.TrimSpace(sizeStr))
	if err != nil {
		return fmt.Errorf("GETSCRIPT literal size %q: %w", litHdr, err)
	}
	got := make([]byte, size)
	if _, err := io.ReadFull(conn, got); err != nil {
		return fmt.Errorf("GETSCRIPT body: %w", err)
	}
	if !bytes.Equal(got, []byte(scriptBody)) {
		return fmt.Errorf("GETSCRIPT content mismatch: got %q want %q", got, scriptBody)
	}
	// Drain trailing CRLF + OK line.
	if err := msieveDrainUntilOK(conn); err != nil {
		return fmt.Errorf("GETSCRIPT trailing: %w", err)
	}

	// SETACTIVE — activate the script.
	fmt.Fprintf(conn, "SETACTIVE %q\r\n", scriptName)
	line, err = readLine(conn)
	if err != nil {
		return fmt.Errorf("SETACTIVE response: %w", err)
	}
	if !strings.HasPrefix(line, "OK") {
		return fmt.Errorf("SETACTIVE failed: %q", line)
	}

	// SETACTIVE "" — deactivate so we can delete.
	fmt.Fprintf(conn, "SETACTIVE \"\"\r\n")
	line, err = readLine(conn)
	if err != nil {
		return fmt.Errorf("SETACTIVE deactivate response: %w", err)
	}
	if !strings.HasPrefix(line, "OK") {
		return fmt.Errorf("SETACTIVE deactivate failed: %q", line)
	}

	// DELETESCRIPT
	fmt.Fprintf(conn, "DELETESCRIPT %q\r\n", scriptName)
	line, err = readLine(conn)
	if err != nil {
		return fmt.Errorf("DELETESCRIPT response: %w", err)
	}
	if !strings.HasPrefix(line, "OK") {
		return fmt.Errorf("DELETESCRIPT failed: %q", line)
	}

	// LOGOUT
	fmt.Fprintf(conn, "LOGOUT\r\n")
	return nil
}

// msieveDrainUntilOK reads and discards ManageSieve response lines until a
// line starting with "OK" is found. Returns an error if "NO" or "BYE" is seen.
func msieveDrainUntilOK(conn net.Conn) error {
	for {
		line, err := readLine(conn)
		if err != nil {
			return err
		}
		if strings.HasPrefix(line, "OK") {
			return nil
		}
		if strings.HasPrefix(line, "NO") || strings.HasPrefix(line, "BYE") {
			return fmt.Errorf("server error: %q", line)
		}
	}
}

// ---- low-level -----------------------------------------------------------

func readLine(r io.Reader) (string, error) {
	var buf []byte
	b := make([]byte, 1)
	for {
		_, err := r.Read(b)
		if err != nil {
			return "", err
		}
		if b[0] == '\n' {
			break
		}
		buf = append(buf, b[0])
	}
	return strings.TrimRight(string(buf), "\r"), nil
}
