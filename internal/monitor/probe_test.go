package monitor

import (
	"fmt"
	"net"
	"testing"
	"time"
)

const testTimeout = 2 * time.Second

// startMockServer starts a mock TCP server, calls handler once, then closes.
// Returns the listen address.
func startMockServer(t *testing.T, handler func(conn net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		handler(conn)
	}()
	return ln.Addr().String()
}

// addrParts splits "ip:port" into components usable by probe functions.
func addrParts(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split host/port %q: %v", addr, err)
	}
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	return host, port
}

// ---- IMAP tests ----

func TestProbeIMAP_OK(t *testing.T) {
	addr := startMockServer(t, func(conn net.Conn) {
		fmt.Fprintf(conn, "* OK IMAP ready\r\n")
		buf := make([]byte, 256)
		n, _ := conn.Read(buf)
		if n > 0 {
			fmt.Fprintf(conn, "M001 OK logged in\r\n")
		}
		conn.Read(buf) //nolint:errcheck
	})
	ip, port := addrParts(t, addr)
	if got := probeIMAP(ip, port, "user@x.com", "secret", testTimeout); got != probeOK {
		t.Fatalf("expected probeOK, got %v", got)
	}
}

func TestProbeIMAP_LoginFail(t *testing.T) {
	addr := startMockServer(t, func(conn net.Conn) {
		fmt.Fprintf(conn, "* OK IMAP ready\r\n")
		buf := make([]byte, 256)
		conn.Read(buf) //nolint:errcheck
		fmt.Fprintf(conn, "M001 NO [AUTHENTICATIONFAILED] invalid credentials\r\n")
	})
	ip, port := addrParts(t, addr)
	if got := probeIMAP(ip, port, "bad@x.com", "wrong", testTimeout); got != probeLogin {
		t.Fatalf("expected probeLogin, got %v", got)
	}
}

func TestProbeIMAP_Refused(t *testing.T) {
	if got := probeIMAP("127.0.0.1", 1, "u", "p", testTimeout); got != probeRefused {
		t.Fatalf("expected probeRefused, got %v", got)
	}
}

func TestProbeIMAP_NoUser(t *testing.T) {
	addr := startMockServer(t, func(conn net.Conn) {
		fmt.Fprintf(conn, "* OK IMAP ready\r\n")
		buf := make([]byte, 64)
		conn.Read(buf) //nolint:errcheck
	})
	ip, port := addrParts(t, addr)
	if got := probeIMAP(ip, port, "", "", testTimeout); got != probeOK {
		t.Fatalf("expected probeOK, got %v", got)
	}
}

// ---- POP3 tests ----

func TestProbePOP3_OK(t *testing.T) {
	addr := startMockServer(t, func(conn net.Conn) {
		fmt.Fprintf(conn, "+OK POP3 ready\r\n")
		buf := make([]byte, 256)
		conn.Read(buf) //nolint:errcheck
		fmt.Fprintf(conn, "+OK user accepted\r\n")
		conn.Read(buf) //nolint:errcheck
		fmt.Fprintf(conn, "+OK logged in\r\n")
		conn.Read(buf) //nolint:errcheck
	})
	ip, port := addrParts(t, addr)
	if got := probePOP3(ip, port, "user@x.com", "secret", testTimeout); got != probeOK {
		t.Fatalf("expected probeOK, got %v", got)
	}
}

func TestProbePOP3_LoginFail(t *testing.T) {
	addr := startMockServer(t, func(conn net.Conn) {
		fmt.Fprintf(conn, "+OK POP3 ready\r\n")
		buf := make([]byte, 256)
		conn.Read(buf) //nolint:errcheck
		fmt.Fprintf(conn, "+OK user accepted\r\n")
		conn.Read(buf) //nolint:errcheck
		fmt.Fprintf(conn, "-ERR invalid password\r\n")
	})
	ip, port := addrParts(t, addr)
	if got := probePOP3(ip, port, "user@x.com", "bad", testTimeout); got != probeLogin {
		t.Fatalf("expected probeLogin, got %v", got)
	}
}

func TestProbePOP3_Refused(t *testing.T) {
	if got := probePOP3("127.0.0.1", 1, "u", "p", testTimeout); got != probeRefused {
		t.Fatalf("expected probeRefused, got %v", got)
	}
}

// ---- LMTP tests ----

func TestProbeLMTP_OK(t *testing.T) {
	addr := startMockServer(t, func(conn net.Conn) {
		fmt.Fprintf(conn, "220 lmtp ready\r\n")
		buf := make([]byte, 256)
		conn.Read(buf) //nolint:errcheck
		fmt.Fprintf(conn, "250-lmtp.example.com\r\n")
		fmt.Fprintf(conn, "250 PIPELINING\r\n")
		conn.Read(buf) //nolint:errcheck
	})
	ip, port := addrParts(t, addr)
	if got := probeLMTP(ip, port, testTimeout); got != probeOK {
		t.Fatalf("expected probeOK, got %v", got)
	}
}

func TestProbeLMTP_Refused(t *testing.T) {
	if got := probeLMTP("127.0.0.1", 1, testTimeout); got != probeRefused {
		t.Fatalf("expected probeRefused, got %v", got)
	}
}

// ---- credentials (Config.credentials) ----

func TestConfig_Credentials_TagMatch(t *testing.T) {
	cfg := &Config{
		Tags: map[string]TagConfig{
			"ssd": {User: "ssd@x.com", Password: "ssdpass"},
			"":    {User: "default@x.com", Password: "defpass"},
		},
	}
	u, p := cfg.credentials("ssd")
	if u != "ssd@x.com" || p != "ssdpass" {
		t.Fatalf("expected ssd creds, got %q %q", u, p)
	}
}

func TestConfig_Credentials_DefaultFallback(t *testing.T) {
	cfg := &Config{
		Tags: map[string]TagConfig{
			"": {User: "default@x.com", Password: "defpass"},
		},
	}
	u, p := cfg.credentials("unknown-tag")
	if u != "default@x.com" || p != "defpass" {
		t.Fatalf("expected default fallback, got %q %q", u, p)
	}
}

func TestConfig_Credentials_NoEntry(t *testing.T) {
	cfg := &Config{}
	u, p := cfg.credentials("ssd")
	if u != "" || p != "" {
		t.Fatalf("expected empty creds, got %q %q", u, p)
	}
}
