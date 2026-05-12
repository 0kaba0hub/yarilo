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
	flagHost      = flag.String("host", "localhost", "yarilo hostname")
	flagIMAPSPort = flag.String("imap-port", "993", "IMAPS port")
	flagTelemetry = flag.String("telemetry", "http://localhost:8080", "telemetry base URL")
	flagTimeout   = flag.Duration("timeout", 10*time.Second, "per-check timeout")
	flagInsecure  = flag.Bool("insecure", false, "skip TLS certificate verification")
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

func checkHealth() error {
	return httpGet(*flagTelemetry + "/healthz")
}

func checkReady() error {
	return httpGet(*flagTelemetry + "/readyz")
}

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

	// Read greeting: * OK ...
	greeting, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("read greeting: %w", err)
	}
	if !strings.HasPrefix(greeting, "* OK") {
		return fmt.Errorf("unexpected greeting: %q", greeting)
	}

	// Send CAPABILITY
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

	// Graceful logout
	fmt.Fprintf(conn, "A002 LOGOUT\r\n")
	return nil
}

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
