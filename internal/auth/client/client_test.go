package client

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
)

// stubAuthServer starts a minimal yarilo-auth server for testing.
// It accepts AUTH commands and responds according to the supplied responder.
func stubAuthServer(t *testing.T, handle func(conn net.Conn, rd *bufio.Reader)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				rd := bufio.NewReader(c)
				fmt.Fprintf(c, "VERSION\t1\t0\n")
				fmt.Fprintf(c, "MECH\tPLAIN\n")
				fmt.Fprintf(c, "DONE\n")
				handle(c, rd)
			}()
		}
	}()
	return ln.Addr().String()
}

func TestDial_HandshakeFail(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	go func() {
		c, _ := ln.Accept()
		// Send garbage instead of VERSION
		fmt.Fprintf(c, "GARBAGE\n")
		c.Close()
	}()

	_, err = Dial(addr, nil)
	if err == nil {
		t.Fatal("expected error on bad handshake, got nil")
	}
}

func TestAuthenticate_OK(t *testing.T) {
	addr := stubAuthServer(t, func(conn net.Conn, rd *bufio.Reader) {
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.SplitN(strings.TrimRight(line, "\r\n"), "\t", 5)
			if len(fields) < 2 || fields[0] == "VERSION" {
				continue
			}
			if fields[0] == "AUTH" {
				id := fields[1]
				fmt.Fprintf(conn, "OK\t%s\tuser=alice\ttoken=abc123def456\n", id)
				return
			}
		}
	})

	c, err := Dial(addr, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	res, err := c.Authenticate("alice", "pass", "imap", "1.2.3.4", "sess1")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if res.Username != "alice" {
		t.Errorf("Username = %q, want %q", res.Username, "alice")
	}
	if res.Token != "abc123def456" {
		t.Errorf("Token = %q, want %q", res.Token, "abc123def456")
	}
}

// TestAuthenticate_DirectorTag proves #746: a director_tag= token on the
// AUTH OK reply lands on AuthResult.DirectorTag.
func TestAuthenticate_DirectorTag(t *testing.T) {
	addr := stubAuthServer(t, func(conn net.Conn, rd *bufio.Reader) {
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.SplitN(strings.TrimRight(line, "\r\n"), "\t", 5)
			if len(fields) < 2 || fields[0] == "VERSION" {
				continue
			}
			if fields[0] == "AUTH" {
				id := fields[1]
				fmt.Fprintf(conn, "OK\t%s\tuser=alice\tdirector_tag=b\ttoken=abc123def456\n", id)
				return
			}
		}
	})

	c, err := Dial(addr, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	res, err := c.Authenticate("alice", "pass", "imap", "1.2.3.4", "sess1")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if res.DirectorTag != "b" {
		t.Errorf("DirectorTag = %q, want %q", res.DirectorTag, "b")
	}
}

func TestAuthenticate_Fail(t *testing.T) {
	addr := stubAuthServer(t, func(conn net.Conn, rd *bufio.Reader) {
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.SplitN(strings.TrimRight(line, "\r\n"), "\t", 5)
			if len(fields) < 2 || fields[0] == "VERSION" {
				continue
			}
			if fields[0] == "AUTH" {
				id := fields[1]
				fmt.Fprintf(conn, "FAIL\t%s\n", id)
				return
			}
		}
	})

	c, err := Dial(addr, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	_, err = c.Authenticate("alice", "wrong", "imap", "", "")
	if err != ErrAuthFailed {
		t.Errorf("err = %v, want ErrAuthFailed", err)
	}
}

func TestAuthenticate_TempFail(t *testing.T) {
	addr := stubAuthServer(t, func(conn net.Conn, rd *bufio.Reader) {
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.SplitN(strings.TrimRight(line, "\r\n"), "\t", 5)
			if len(fields) < 2 || fields[0] == "VERSION" {
				continue
			}
			if fields[0] == "AUTH" {
				id := fields[1]
				fmt.Fprintf(conn, "FAIL\t%s\ttemp_fail\n", id)
				return
			}
		}
	})

	c, err := Dial(addr, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	_, err = c.Authenticate("alice", "pass", "imap", "", "")
	if err != ErrTempFail {
		t.Errorf("err = %v, want ErrTempFail", err)
	}
}

func TestVerify_OK(t *testing.T) {
	addr := stubAuthServer(t, func(conn net.Conn, rd *bufio.Reader) {
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.SplitN(strings.TrimRight(line, "\r\n"), "\t", 4)
			if len(fields) < 2 || fields[0] == "VERSION" {
				continue
			}
			if fields[0] == "VERIFY" {
				id := fields[1]
				fmt.Fprintf(conn, "OK\t%s\tuser=alice\tsession=sess42\tservice=imap\n", id)
				return
			}
		}
	})

	c, err := Dial(addr, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	username, sessionID, service, err := c.Verify("sometoken", "alice", "sess42")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if username != "alice" {
		t.Errorf("username = %q, want %q", username, "alice")
	}
	if sessionID != "sess42" {
		t.Errorf("sessionID = %q, want %q", sessionID, "sess42")
	}
	if service != "imap" {
		t.Errorf("service = %q, want %q", service, "imap")
	}
}

func TestVerify_Fail(t *testing.T) {
	addr := stubAuthServer(t, func(conn net.Conn, rd *bufio.Reader) {
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.SplitN(strings.TrimRight(line, "\r\n"), "\t", 4)
			if len(fields) < 2 || fields[0] == "VERSION" {
				continue
			}
			if fields[0] == "VERIFY" {
				id := fields[1]
				fmt.Fprintf(conn, "FAIL\t%s\n", id)
				return
			}
		}
	})

	c, err := Dial(addr, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	_, _, _, err = c.Verify("badtoken", "", "")
	if err != ErrAuthFailed {
		t.Errorf("err = %v, want ErrAuthFailed", err)
	}
}

func TestAuthenticate_Nologin(t *testing.T) {
	addr := stubAuthServer(t, func(conn net.Conn, rd *bufio.Reader) {
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.SplitN(strings.TrimRight(line, "\r\n"), "\t", 5)
			if len(fields) < 2 || fields[0] == "VERSION" {
				continue
			}
			if fields[0] == "AUTH" {
				id := fields[1]
				fmt.Fprintf(conn, "OK\t%s\tuser=disabled\tnologin\n", id)
				return
			}
		}
	})

	c, err := Dial(addr, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	res, err := c.Authenticate("disabled", "pass", "imap", "", "")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !res.Nologin {
		t.Error("Nologin = false, want true")
	}
}
