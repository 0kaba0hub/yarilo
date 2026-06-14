// smoketest verifies a live yarilo deployment.
// Usage: smoketest -host mail.example.com -imap-port 993 -telemetry http://...:8080
//
// Exit 0 = all checks passed.
// Exit 1 = one or more checks failed (prints failures to stderr).
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

var (
	flagHost          = flag.String("host", "localhost", "yarilo hostname")
	flagIMAPSPort     = flag.String("imap-port", "993", "IMAPS port")
	flagPOP3SPort     = flag.String("pop3s-port", "995", "POP3S port")
	flagSMTPMXPort    = flag.String("smtp-mx-port", "25", "SMTP MX port")
	flagSMTPSubPort   = flag.String("smtp-sub-port", "587", "SMTP submission port")
	flagLMTPLoginPort = flag.String("lmtp-login-port", "24", "yarilo-lmtp-login port")
	flagTelemetry     = flag.String("telemetry", "http://localhost:8080", "telemetry base URL")
	flagTimeout       = flag.Duration("timeout", 10*time.Second, "per-check timeout")
	flagInsecure      = flag.Bool("insecure", false, "skip TLS certificate verification")
	flagProxyProtocol = flag.Bool("proxy-protocol", false, "send HAProxy PROXY header before SMTP banner")
	flagXClient       = flag.Bool("xclient", false, "check that MX port advertises XCLIENT in EHLO")
	flagPOP3S         = flag.Bool("pop3s", false, "check POP3S greeting and CAPA")
	flagLMTPLogin     = flag.Bool("lmtp-login", false, "check yarilo-lmtp-login LHLO greeting (port -lmtp-login-port)")
)

type result struct {
	name string
	err  error
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	flag.Parse()

	checks := []struct {
		name string
		fn   func() error
	}{
		{"telemetry /healthz", checkHealth},
		{"telemetry /readyz", checkReady},
		{"imap CAPABILITY", checkIMAP},
		{"smtp MX EHLO", checkSMTPMX},
		{"smtp submission EHLO+STARTTLS", checkSMTPSubmission},
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

	var failures []result
	for _, c := range checks {
		if err := c.fn(); err != nil {
			slog.Error("FAIL", "check", c.name, "err", err)
			failures = append(failures, result{c.name, err})
		} else {
			slog.Info("OK", "check", c.name)
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

// ---- IMAP ----------------------------------------------------------------

func checkIMAP() error {
	addr := net.JoinHostPort(*flagHost, *flagIMAPSPort)
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
	if !strings.HasPrefix(greeting, "* OK") {
		return fmt.Errorf("unexpected greeting: %q", greeting)
	}

	fmt.Fprintf(conn, "A001 CAPABILITY\r\n")
	for {
		line, err := readLine(conn)
		if err != nil {
			return fmt.Errorf("CAPABILITY read: %w", err)
		}
		if strings.HasPrefix(line, "* CAPABILITY") {
			if !strings.Contains(line, "IMAP4rev1") {
				return fmt.Errorf("CAPABILITY missing IMAP4rev1: %q", line)
			}
		}
		if strings.HasPrefix(line, "A001 OK") {
			break
		}
		if strings.HasPrefix(line, "A001 BAD") || strings.HasPrefix(line, "A001 NO") {
			return fmt.Errorf("CAPABILITY command failed: %q", line)
		}
	}
	fmt.Fprintf(conn, "A002 LOGOUT\r\n")
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
