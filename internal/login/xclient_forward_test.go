package login

import (
	"bufio"
	"encoding/base64"
	"net"
	"strings"
	"testing"
)

func TestIDForwardedClientIP(t *testing.T) {
	cases := []struct {
		name     string
		line     string
		wantIP   string
		wantPort string
	}{
		{"originating ip+port", `a ID ("x-originating-ip" "203.0.113.9" "x-originating-port" "54321")`, "203.0.113.9", "54321"},
		{"client-ip alias", `a ID ("x-client-ip" "198.51.100.7")`, "198.51.100.7", ""},
		{"ipv6 bracketed", `a ID ("x-originating-ip" "[2001:db8::1]")`, "2001:db8::1", ""},
		{"nil params", `a ID NIL`, "", ""},
		{"unrelated keys", `a ID ("name" "mutt" "version" "2.0")`, "", ""},
		{"no parens", `a ID`, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ip, port := idForwardedClientIP(tc.line)
			if ip != tc.wantIP || port != tc.wantPort {
				t.Fatalf("idForwardedClientIP(%q) = (%q,%q), want (%q,%q)", tc.line, ip, port, tc.wantIP, tc.wantPort)
			}
		})
	}
}

func TestXClientForwarded(t *testing.T) {
	cases := []struct {
		name     string
		line     string
		wantIP   string
		wantPort string
	}{
		{"addr+port", "XCLIENT ADDR=203.0.113.9 PORT=25000", "203.0.113.9", "25000"},
		{"addr only", "XCLIENT ADDR=198.51.100.7", "198.51.100.7", ""},
		{"unavailable", "XCLIENT ADDR=[UNAVAILABLE]", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ip, port := xclientForwarded(tc.line)
			if ip != tc.wantIP || port != tc.wantPort {
				t.Fatalf("xclientForwarded(%q) = (%q,%q), want (%q,%q)", tc.line, ip, port, tc.wantIP, tc.wantPort)
			}
		})
	}
}

func TestIPInNets(t *testing.T) {
	_, n10, _ := net.ParseCIDR("10.0.0.0/8")
	_, nLocal, _ := net.ParseCIDR("127.0.0.1/32")
	nets := []*net.IPNet{n10, nLocal}
	cases := []struct {
		ip   string
		want bool
	}{
		{"10.1.2.3", true},
		{"127.0.0.1", true},
		{"203.0.113.9", false},
		{"not-an-ip", false},
	}
	for _, tc := range cases {
		if got := ipInNets(tc.ip, nets); got != tc.want {
			t.Errorf("ipInNets(%q) = %v, want %v", tc.ip, got, tc.want)
		}
	}
	if ipInNets("10.1.2.3", nil) {
		t.Errorf("empty nets must trust nobody")
	}
}

// TestIMAPPreamble_ForwardedID: an ID with x-originating-ip is captured into the
// preamble when the listener has XClient enabled, and ignored when it does not.
func TestIMAPPreamble_ForwardedID(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			srv, cli := pipePair(t)
			errCh := make(chan error, 1)
			var got *preamble
			go func() {
				rd := bufio.NewReader(srv)
				p, _, _, err := extractIMAPPreamble(srv, rd, nil, Options{XClient: enabled})
				got = p
				errCh <- err
			}()
			crd := bufio.NewReader(cli)
			crd.ReadString('\n') // greeting
			cli.Write([]byte(`A0 ID ("x-originating-ip" "203.0.113.9" "x-originating-port" "54321")` + "\r\n"))
			crd.ReadString('\n') // * ID NIL
			crd.ReadString('\n') // A0 OK ID
			cli.Write([]byte("A1 LOGIN alice \"pw\"\r\n"))
			if err := <-errCh; err != nil {
				t.Fatalf("extractIMAPPreamble: %v", err)
			}
			wantIP := "203.0.113.9"
			if !enabled {
				wantIP = ""
			}
			if got.forwardIP != wantIP {
				t.Fatalf("forwardIP = %q, want %q (enabled=%v)", got.forwardIP, wantIP, enabled)
			}
			if enabled && (got.forwardPort != "54321" || got.forwardSource != "id") {
				t.Fatalf("forwardPort/source = %q/%q, want 54321/id", got.forwardPort, got.forwardSource)
			}
		})
	}
}

// TestPOP3Preamble_ForwardedXClient: XCLIENT ADDR is captured with XClient on.
func TestPOP3Preamble_ForwardedXClient(t *testing.T) {
	srv, cli := pipePair(t)
	errCh := make(chan error, 1)
	var got *preamble
	go func() {
		rd := bufio.NewReader(srv)
		p, _, _, err := extractPOP3Preamble(srv, rd, nil, Options{XClient: true})
		got = p
		errCh <- err
	}()
	crd := bufio.NewReader(cli)
	crd.ReadString('\n') // +OK greeting
	cli.Write([]byte("XCLIENT ADDR=203.0.113.9 PORT=25000\r\n"))
	crd.ReadString('\n') // +OK
	cli.Write([]byte("USER alice\r\n"))
	crd.ReadString('\n') // +OK
	cli.Write([]byte("PASS pw\r\n"))
	if err := <-errCh; err != nil {
		t.Fatalf("extractPOP3Preamble: %v", err)
	}
	if got.forwardIP != "203.0.113.9" || got.forwardSource != "xclient" {
		t.Fatalf("forwardIP/source = %q/%q, want 203.0.113.9/xclient", got.forwardIP, got.forwardSource)
	}
}

// TestPOP3Preamble_XClientDisabledUnknown: with XClient off, XCLIENT is an
// unknown command and nothing is captured.
func TestPOP3Preamble_XClientDisabledUnknown(t *testing.T) {
	srv, cli := pipePair(t)
	errCh := make(chan error, 1)
	var got *preamble
	go func() {
		rd := bufio.NewReader(srv)
		p, _, _, err := extractPOP3Preamble(srv, rd, nil, Options{})
		got = p
		errCh <- err
	}()
	crd := bufio.NewReader(cli)
	crd.ReadString('\n') // +OK greeting
	cli.Write([]byte("XCLIENT ADDR=203.0.113.9\r\n"))
	resp, _ := crd.ReadString('\n')
	if !strings.HasPrefix(resp, "-ERR") {
		t.Fatalf("XCLIENT with xclient off must be -ERR, got %q", resp)
	}
	cli.Write([]byte("USER alice\r\n"))
	crd.ReadString('\n')
	cli.Write([]byte("PASS pw\r\n"))
	if err := <-errCh; err != nil {
		t.Fatalf("extractPOP3Preamble: %v", err)
	}
	if got.forwardIP != "" {
		t.Fatalf("forwardIP must be empty when xclient off, got %q", got.forwardIP)
	}
}

// TestSubmissionPreamble_ForwardedXClient: EHLO advertises XCLIENT, an inbound
// XCLIENT resets to post-greeting (re-EHLO), and the ADDR is captured.
func TestSubmissionPreamble_ForwardedXClient(t *testing.T) {
	srv, cli := pipePair(t)
	errCh := make(chan error, 1)
	var got *preamble
	go func() {
		rd := bufio.NewReader(srv)
		p, _, _, err := extractSubmissionPreamble(srv, rd, nil, Options{XClient: true})
		got = p
		errCh <- err
	}()
	crd := bufio.NewReader(cli)
	crd.ReadString('\n') // 220 greeting

	cli.Write([]byte("EHLO relay\r\n"))
	sawXClient := readEHLOCaps(t, crd)
	if !sawXClient {
		t.Fatalf("EHLO must advertise XCLIENT when enabled")
	}

	cli.Write([]byte("XCLIENT ADDR=203.0.113.9\r\n"))
	reset, _ := crd.ReadString('\n')
	if !strings.HasPrefix(reset, "220") {
		t.Fatalf("XCLIENT reset reply must be 220, got %q", reset)
	}

	cli.Write([]byte("EHLO relay\r\n"))
	readEHLOCaps(t, crd)

	b64 := base64.StdEncoding.EncodeToString([]byte("\x00alice\x00pw"))
	cli.Write([]byte("AUTH PLAIN " + b64 + "\r\n"))

	if err := <-errCh; err != nil {
		t.Fatalf("extractSubmissionPreamble: %v", err)
	}
	if got.forwardIP != "203.0.113.9" || got.forwardSource != "xclient" {
		t.Fatalf("forwardIP/source = %q/%q, want 203.0.113.9/xclient", got.forwardIP, got.forwardSource)
	}
	if got.username != "alice" {
		t.Fatalf("username = %q, want alice", got.username)
	}
}

// readEHLOCaps reads a multi-line SMTP EHLO reply (250-… lines then a final
// 250 line) and reports whether any line advertised XCLIENT.
func readEHLOCaps(t *testing.T, rd *bufio.Reader) bool {
	t.Helper()
	saw := false
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("read ehlo caps: %v", err)
		}
		if strings.Contains(strings.ToUpper(line), "XCLIENT") {
			saw = true
		}
		if strings.HasPrefix(line, "250 ") { // final line (space, not dash)
			return saw
		}
	}
}
