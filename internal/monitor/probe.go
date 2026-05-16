package monitor

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"time"
)

type probeResult int

const (
	probeOK      probeResult = iota
	probeRefused             // connection refused or timeout
	probeLogin               // connected but login failed
)

// probeIMAP dials ip:port and performs a plain-text IMAP LOGIN check.
// An empty user skips the login step and returns probeOK after greeting.
func probeIMAP(ip string, port int, user, pass string, timeout time.Duration) probeResult {
	addr := fmt.Sprintf("%s:%d", ip, port)
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return probeRefused
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout)) //nolint:errcheck

	sc := bufio.NewScanner(conn)
	// Read greeting: must start with "* OK"
	if !sc.Scan() || !strings.HasPrefix(sc.Text(), "* OK") {
		return probeRefused
	}

	if user == "" {
		return probeOK
	}

	// Send LOGIN
	tag := "M001"
	fmt.Fprintf(conn, "%s LOGIN %s %s\r\n", tag, imapQuote(user), imapQuote(pass)) //nolint:errcheck
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, tag+" OK") {
			fmt.Fprintf(conn, "M002 LOGOUT\r\n") //nolint:errcheck
			return probeOK
		}
		if strings.HasPrefix(line, tag+" NO") || strings.HasPrefix(line, tag+" BAD") {
			return probeLogin
		}
		// tagged continuation line, keep reading
	}
	return probeLogin
}

// probePOP3 dials ip:port and performs a POP3 USER/PASS check.
// An empty user skips authentication and returns probeOK after greeting.
func probePOP3(ip string, port int, user, pass string, timeout time.Duration) probeResult {
	addr := fmt.Sprintf("%s:%d", ip, port)
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return probeRefused
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout)) //nolint:errcheck

	sc := bufio.NewScanner(conn)
	if !sc.Scan() || !strings.HasPrefix(sc.Text(), "+OK") {
		return probeRefused
	}

	if user == "" {
		return probeOK
	}

	fmt.Fprintf(conn, "USER %s\r\n", user) //nolint:errcheck
	if !sc.Scan() || !strings.HasPrefix(sc.Text(), "+OK") {
		return probeLogin
	}

	fmt.Fprintf(conn, "PASS %s\r\n", pass) //nolint:errcheck
	if !sc.Scan() || !strings.HasPrefix(sc.Text(), "+OK") {
		return probeLogin
	}

	fmt.Fprintf(conn, "QUIT\r\n") //nolint:errcheck
	return probeOK
}

// probeLMTP dials ip:port, reads the 220 greeting, and sends LHLO + QUIT.
// LMTP has no authentication at the connection level; probing login requires
// delivering a test message. For a lightweight health check, verifying that
// the server responds to LHLO is sufficient.
func probeLMTP(ip string, port int, timeout time.Duration) probeResult {
	addr := fmt.Sprintf("%s:%d", ip, port)
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return probeRefused
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout)) //nolint:errcheck

	sc := bufio.NewScanner(conn)
	// Read greeting: "220 ..."
	if !sc.Scan() || !strings.HasPrefix(sc.Text(), "220") {
		return probeRefused
	}

	fmt.Fprintf(conn, "LHLO yarilo-monitor\r\n") //nolint:errcheck
	// Consume multi-line 250 response
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "250 ") {
			// last line of EHLO/LHLO response
			fmt.Fprintf(conn, "QUIT\r\n") //nolint:errcheck
			return probeOK
		}
		if strings.HasPrefix(line, "250-") {
			continue
		}
		return probeRefused
	}
	return probeRefused
}

// imapQuote wraps a string in IMAP literal or quoted form.
// Simple ASCII strings are quoted; avoids injection via backslash-escaping.
func imapQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
