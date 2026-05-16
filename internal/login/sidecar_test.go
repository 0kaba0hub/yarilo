package login

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
)

// TestSidecarIMAP verifies the full sidecar handshake for IMAP:
// frontend sends greeting → XCLIENT → session connected → biProxy starts.
func TestSidecarIMAP(t *testing.T) {
	// Simulate yarilo-imap session process.
	sessionLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sessionLn.Close()

	sessionDone := make(chan struct{})
	var receivedXCLIENT string
	go func() {
		defer close(sessionDone)
		conn, err := sessionLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Send IMAP greeting.
		fmt.Fprintf(conn, "* OK session ready\r\n")
		rd := bufio.NewReader(conn)
		// Read XCLIENT from sidecar.
		line, _ := rd.ReadString('\n')
		receivedXCLIENT = strings.TrimRight(line, "\r\n")
		// Ack XCLIENT.
		fmt.Fprintf(conn, "XCONN OK XCLIENT\r\n")
		// Simple echo for biProxy test: read one line, write it back.
		echo, _ := rd.ReadString('\n')
		fmt.Fprint(conn, echo)
	}()

	// Start the sidecar.
	sidecarLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sidecarLn.Close()

	sidecar := NewSidecar(SidecarOptions{
		Protocol:    ProtocolIMAPS,
		SessionAddr: sessionLn.Addr().String(),
	})
	go sidecar.Serve(sidecarLn) //nolint:errcheck

	// Act as the frontend login pod.
	frontConn, err := net.Dial("tcp", sidecarLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer frontConn.Close()
	frontRd := bufio.NewReader(frontConn)

	// 1. Read and discard sidecar greeting.
	greeting, err := frontRd.ReadString('\n')
	if err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	if !strings.HasPrefix(greeting, "* OK") {
		t.Errorf("expected IMAP greeting, got %q", greeting)
	}

	// 2. Send XCLIENT.
	fmt.Fprintf(frontConn, "XCONN XCLIENT ADDR=1.2.3.4\r\n")
	ack, err := frontRd.ReadString('\n')
	if err != nil {
		t.Fatalf("read xclient ack: %v", err)
	}
	if !strings.Contains(ack, "OK XCLIENT") {
		t.Errorf("expected OK XCLIENT ack, got %q", ack)
	}

	// 3. biProxy: send a line through, get it echoed back.
	fmt.Fprintf(frontConn, "A001 CAPABILITY\r\n")
	echo, err := frontRd.ReadString('\n')
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !strings.Contains(echo, "A001 CAPABILITY") {
		t.Errorf("expected echo, got %q", echo)
	}

	<-sessionDone

	if !strings.Contains(receivedXCLIENT, "ADDR=1.2.3.4") {
		t.Errorf("session did not receive XCLIENT ADDR=1.2.3.4, got %q", receivedXCLIENT)
	}
}

// TestSidecarPOP3 verifies the sidecar handshake for POP3.
func TestSidecarPOP3(t *testing.T) {
	sessionLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sessionLn.Close()

	sessionDone := make(chan struct{})
	var receivedXCLIENT string
	go func() {
		defer close(sessionDone)
		conn, err := sessionLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintf(conn, "+OK session ready\r\n")
		rd := bufio.NewReader(conn)
		line, _ := rd.ReadString('\n')
		receivedXCLIENT = strings.TrimRight(line, "\r\n")
		fmt.Fprintf(conn, "+OK XCLIENT accepted\r\n")
		echo, _ := rd.ReadString('\n')
		fmt.Fprint(conn, echo)
	}()

	sidecarLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sidecarLn.Close()

	sidecar := NewSidecar(SidecarOptions{
		Protocol:    ProtocolPOP3S,
		SessionAddr: sessionLn.Addr().String(),
	})
	go sidecar.Serve(sidecarLn) //nolint:errcheck

	frontConn, err := net.Dial("tcp", sidecarLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer frontConn.Close()
	frontRd := bufio.NewReader(frontConn)

	greeting, err := frontRd.ReadString('\n')
	if err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	if !strings.HasPrefix(greeting, "+OK") {
		t.Errorf("expected POP3 greeting, got %q", greeting)
	}

	fmt.Fprintf(frontConn, "XCLIENT ADDR=5.6.7.8\r\n")
	ack, err := frontRd.ReadString('\n')
	if err != nil {
		t.Fatalf("read xclient ack: %v", err)
	}
	if !strings.HasPrefix(ack, "+OK") {
		t.Errorf("expected +OK ack, got %q", ack)
	}

	fmt.Fprintf(frontConn, "USER alice\r\n")
	echo, err := frontRd.ReadString('\n')
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !strings.Contains(echo, "USER alice") {
		t.Errorf("expected echo, got %q", echo)
	}

	<-sessionDone

	if !strings.Contains(receivedXCLIENT, "ADDR=5.6.7.8") {
		t.Errorf("session did not receive XCLIENT ADDR=5.6.7.8, got %q", receivedXCLIENT)
	}
}

// TestSidecarSMTP verifies the sidecar handshake for Submission.
// EHLO must NOT be sent by the sidecar — it flows through biProxy from the frontend.
func TestSidecarSMTP(t *testing.T) {
	sessionLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sessionLn.Close()

	sessionDone := make(chan struct{})
	var receivedLines []string
	go func() {
		defer close(sessionDone)
		conn, err := sessionLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Send SMTP greeting (multi-line not needed for greeting discard test).
		fmt.Fprintf(conn, "220 session ready\r\n")
		rd := bufio.NewReader(conn)
		// Read XCLIENT from sidecar.
		line, _ := rd.ReadString('\n')
		receivedLines = append(receivedLines, strings.TrimRight(line, "\r\n"))
		// Ack XCLIENT.
		fmt.Fprintf(conn, "250 OK\r\n")
		// Read next line from biProxy (should be EHLO from frontend, NOT sent by sidecar).
		next, _ := rd.ReadString('\n')
		receivedLines = append(receivedLines, strings.TrimRight(next, "\r\n"))
		// Respond to EHLO with single line (no capabilities for test simplicity).
		fmt.Fprintf(conn, "250 session.local\r\n")
	}()

	sidecarLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sidecarLn.Close()

	sidecar := NewSidecar(SidecarOptions{
		Protocol:    ProtocolSubmission,
		SessionAddr: sessionLn.Addr().String(),
	})
	go sidecar.Serve(sidecarLn) //nolint:errcheck

	frontConn, err := net.Dial("tcp", sidecarLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer frontConn.Close()
	frontRd := bufio.NewReader(frontConn)

	// 1. Read and discard sidecar greeting.
	greeting, err := frontRd.ReadString('\n')
	if err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	if !strings.HasPrefix(greeting, "220") {
		t.Errorf("expected SMTP greeting, got %q", greeting)
	}

	// 2. Send XCLIENT.
	fmt.Fprintf(frontConn, "XCLIENT ADDR=9.10.11.12\r\n")
	ack, err := frontRd.ReadString('\n')
	if err != nil {
		t.Fatalf("read xclient ack: %v", err)
	}
	if !strings.HasPrefix(ack, "250") {
		t.Errorf("expected 250 ack, got %q", ack)
	}

	// 3. biProxy starts. Frontend sends EHLO — should flow through to session.
	fmt.Fprintf(frontConn, "EHLO mail.client\r\n")
	ehloResp, err := frontRd.ReadString('\n')
	if err != nil {
		t.Fatalf("read ehlo response: %v", err)
	}
	if !strings.HasPrefix(ehloResp, "250") {
		t.Errorf("expected 250 ehlo response, got %q", ehloResp)
	}

	<-sessionDone

	if len(receivedLines) < 2 {
		t.Fatalf("session received %d lines, want at least 2", len(receivedLines))
	}
	if !strings.Contains(receivedLines[0], "ADDR=9.10.11.12") {
		t.Errorf("first session line: want XCLIENT ADDR=9.10.11.12, got %q", receivedLines[0])
	}
	if !strings.Contains(receivedLines[1], "EHLO") {
		t.Errorf("second session line: want EHLO (from frontend via biProxy), got %q", receivedLines[1])
	}
}

// TestSidecarIMAPNoClientIP verifies that the sidecar skips XCLIENT forwarding
// when the frontend sends an XCLIENT with no ADDR field.
func TestSidecarIMAPNoClientIP(t *testing.T) {
	sessionLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sessionLn.Close()

	sessionDone := make(chan struct{})
	var receivedFirst string
	go func() {
		defer close(sessionDone)
		conn, err := sessionLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintf(conn, "* OK session ready\r\n")
		rd := bufio.NewReader(conn)
		// Without XCLIENT the first line from sidecar should be the auth replay.
		line, _ := rd.ReadString('\n')
		receivedFirst = strings.TrimRight(line, "\r\n")
		fmt.Fprint(conn, line)
	}()

	sidecarLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sidecarLn.Close()

	sidecar := NewSidecar(SidecarOptions{
		Protocol:    ProtocolIMAPS,
		SessionAddr: sessionLn.Addr().String(),
	})
	go sidecar.Serve(sidecarLn) //nolint:errcheck

	frontConn, err := net.Dial("tcp", sidecarLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer frontConn.Close()
	frontRd := bufio.NewReader(frontConn)

	// Discard greeting.
	frontRd.ReadString('\n') //nolint:errcheck

	// Send XCLIENT without ADDR.
	fmt.Fprintf(frontConn, "XCONN XCLIENT\r\n")
	frontRd.ReadString('\n') //nolint:errcheck // ack

	// biProxy: send login command.
	fmt.Fprintf(frontConn, "A001 LOGIN user pass\r\n")
	frontRd.ReadString('\n') //nolint:errcheck

	<-sessionDone

	// Session should receive the login command directly (no XCLIENT line).
	if strings.Contains(receivedFirst, "XCLIENT") {
		t.Errorf("session received unexpected XCLIENT: %q", receivedFirst)
	}
	if !strings.Contains(receivedFirst, "A001 LOGIN") {
		t.Errorf("expected A001 LOGIN as first session line, got %q", receivedFirst)
	}
}
