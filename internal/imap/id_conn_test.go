package imap

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func newIDImapTestConn(t *testing.T) (client net.Conn, server *idImapConn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		conn *idImapConn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := ln.Accept()
		ln.Close() //nolint:errcheck
		if err != nil {
			ch <- result{nil, err}
			return
		}
		ch <- result{
			&idImapConn{Conn: c, br: bufio.NewReader(c), serverResp: buildIDResponse([]string{"name", "yarilo"})},
			nil,
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	t.Cleanup(func() {
		c.Close()      //nolint:errcheck
		r.conn.Close() //nolint:errcheck
	})
	return c, r.conn
}

func TestIDConn_InterceptsIDCommand(t *testing.T) {
	client, srv := newIDImapTestConn(t)
	d := time.Now().Add(5 * time.Second)
	client.SetDeadline(d) //nolint:errcheck
	srv.SetDeadline(d)    //nolint:errcheck

	cr := bufio.NewReader(client)

	fmt.Fprintf(client, "A1 ID (\"name\" \"tb\")\r\n") //nolint:errcheck
	fmt.Fprintf(client, "A2 NOOP\r\n")                 //nolint:errcheck

	// Server processes lines; A2 NOOP must pass through.
	buf := make([]byte, 256)
	n, err := srv.Read(buf)
	if err != nil && n == 0 {
		t.Fatalf("unexpected error reading non-ID command: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "A2 NOOP") {
		t.Errorf("non-ID command must pass through, got %q", string(buf[:n]))
	}

	// Client must have received the ID response.
	line1 := readLineIMAP(t, cr)
	if !strings.HasPrefix(line1, "* ID") {
		t.Errorf("expected * ID untagged response, got %q", line1)
	}
	line2 := readLineIMAP(t, cr)
	if line2 != "A1 OK ID completed" {
		t.Errorf("expected A1 OK ID completed, got %q", line2)
	}
}

func TestIDConn_IDNil(t *testing.T) {
	client, srv := newIDImapTestConn(t)
	d := time.Now().Add(5 * time.Second)
	client.SetDeadline(d) //nolint:errcheck
	srv.SetDeadline(d)    //nolint:errcheck

	cr := bufio.NewReader(client)

	fmt.Fprintf(client, "A1 ID NIL\r\n") //nolint:errcheck
	fmt.Fprintf(client, "A2 NOOP\r\n")   //nolint:errcheck

	buf := make([]byte, 256)
	n, _ := srv.Read(buf)
	if !strings.Contains(string(buf[:n]), "A2 NOOP") {
		t.Errorf("NOOP must pass through after ID NIL, got %q", string(buf[:n]))
	}

	line1 := readLineIMAP(t, cr)
	if !strings.HasPrefix(line1, "* ID") {
		t.Errorf("expected * ID response for ID NIL, got %q", line1)
	}
}

func TestIDConn_NonIDPassthrough(t *testing.T) {
	client, srv := newIDImapTestConn(t)
	d := time.Now().Add(5 * time.Second)
	client.SetDeadline(d) //nolint:errcheck
	srv.SetDeadline(d)    //nolint:errcheck

	fmt.Fprintf(client, "A1 LOGIN user pass\r\n") //nolint:errcheck

	buf := make([]byte, 256)
	n, err := srv.Read(buf)
	if err != nil && n == 0 {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "A1 LOGIN") {
		t.Errorf("non-ID command must pass through, got %q", string(buf[:n]))
	}
}

func TestIDConn_InjectsIDInCapability(t *testing.T) {
	client, srv := newIDImapTestConn(t)
	d := time.Now().Add(5 * time.Second)
	client.SetDeadline(d) //nolint:errcheck
	srv.SetDeadline(d)    //nolint:errcheck

	cr := bufio.NewReader(client)

	srv.Write([]byte("* CAPABILITY IMAP4rev1 IMAP4rev2 IDLE\r\n")) //nolint:errcheck

	line := readLineIMAP(t, cr)
	if !strings.Contains(line, " ID") {
		t.Errorf("expected ID in CAPABILITY, got %q", line)
	}
}

func TestIDConn_NoDoubleInjectID(t *testing.T) {
	client, srv := newIDImapTestConn(t)
	d := time.Now().Add(5 * time.Second)
	client.SetDeadline(d) //nolint:errcheck
	srv.SetDeadline(d)    //nolint:errcheck

	cr := bufio.NewReader(client)

	srv.Write([]byte("* CAPABILITY IMAP4rev1 ID IMAP4rev2\r\n")) //nolint:errcheck

	line := readLineIMAP(t, cr)
	count := strings.Count(line, " ID")
	if count != 1 {
		t.Errorf("expected exactly 1 ' ID' token, got %d: %q", count, line)
	}
}

func TestParseIDSend_StarExpansion(t *testing.T) {
	pairs := parseIDSend("name * version *")
	m := make(map[string]string)
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	if m["name"] != "yarilo" {
		t.Errorf("expected name=yarilo, got %q", m["name"])
	}
	if m["version"] != "dev" {
		t.Errorf("expected version=dev, got %q", m["version"])
	}
}

func TestParseIDSend_Empty(t *testing.T) {
	if len(parseIDSend("")) != 0 {
		t.Error("empty string should return no pairs")
	}
}

func TestBuildIDResponse(t *testing.T) {
	resp := string(buildIDResponse([]string{"name", "yarilo", "version", "dev"}))
	want := "* ID (\"name\" \"yarilo\" \"version\" \"dev\")\r\n"
	if resp != want {
		t.Errorf("got %q, want %q", resp, want)
	}
}
