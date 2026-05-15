package protocol

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// stubPassdb is a test passdb that returns a preset result.
type stubPassdb struct {
	res *AuthResponse
	err error
}

func (s *stubPassdb) Authenticate(_, _, _ string) (*AuthResponse, error) {
	return s.res, s.err
}

func TestChain_FirstWins(t *testing.T) {
	ok := &stubPassdb{res: &AuthResponse{Result: AuthOK, Username: "alice"}}
	never := &stubPassdb{res: &AuthResponse{Result: AuthFail}}
	chain := Chain{ok, never}

	res, err := chain.Authenticate("alice", "pass", "imap")
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != AuthOK || res.Username != "alice" {
		t.Fatalf("got %+v", res)
	}
}

func TestChain_SkipNil(t *testing.T) {
	// First passdb returns nil (unknown user) → chain must try second.
	skip := &stubPassdb{res: nil}
	ok := &stubPassdb{res: &AuthResponse{Result: AuthOK, Username: "bob"}}
	chain := Chain{skip, ok}

	res, err := chain.Authenticate("bob", "pass", "imap")
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != AuthOK {
		t.Fatalf("got %+v", res)
	}
}

func TestChain_AllUnknown_ReturnsFail(t *testing.T) {
	chain := Chain{
		&stubPassdb{res: nil},
		&stubPassdb{res: nil},
	}
	res, err := chain.Authenticate("nobody", "pass", "imap")
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != AuthFail {
		t.Fatalf("expected AuthFail, got %+v", res)
	}
}

func TestChain_TempFailStopsChain(t *testing.T) {
	fail := &stubPassdb{res: &AuthResponse{Result: AuthTempFail}}
	never := &stubPassdb{res: &AuthResponse{Result: AuthOK, Username: "x"}}
	chain := Chain{fail, never}

	res, _ := chain.Authenticate("x", "pass", "imap")
	if res.Result != AuthTempFail {
		t.Fatalf("expected TempFail propagation, got %+v", res)
	}
}

func TestChain_Empty(t *testing.T) {
	chain := Chain{}
	res, err := chain.Authenticate("x", "pass", "imap")
	if err != nil || res.Result != AuthFail {
		t.Fatalf("empty chain should return AuthFail without error, got res=%+v err=%v", res, err)
	}
}

// credPassdb accepts only a fixed username+password pair.
type credPassdb struct{ user, pass string }

func (c *credPassdb) Authenticate(username, password, _ string) (*AuthResponse, error) {
	if username == c.user && password == c.pass {
		return &AuthResponse{Result: AuthOK, Username: username, Home: "/mail/" + username}, nil
	}
	return nil, nil
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// readHandshake drains server handshake lines until "DONE" and returns all
// lines as a single string. Uses a bufio.Scanner so partial TCP reads are
// handled correctly.
func readHandshake(t *testing.T, sc *bufio.Scanner) string {
	t.Helper()
	var sb strings.Builder
	for sc.Scan() {
		line := sc.Text()
		sb.WriteString(line)
		sb.WriteByte('\n')
		if line == "DONE" {
			break
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanner error reading handshake: %v", err)
	}
	return sb.String()
}

func TestListenAndServe_Handshake(t *testing.T) {
	srv := NewServer(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))

	sc := bufio.NewScanner(conn)
	hs := readHandshake(t, sc)
	if !strings.Contains(hs, "VERSION\tyarilo-auth") {
		t.Errorf("missing VERSION line in handshake: %q", hs)
	}
	if !strings.Contains(hs, "DONE") {
		t.Errorf("missing DONE line in handshake: %q", hs)
	}
}

func TestListenAndServe_AuthOK(t *testing.T) {
	srv := NewServer([]Passdb{&credPassdb{"alice", "secret"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))

	sc := bufio.NewScanner(conn)
	readHandshake(t, sc)

	fmt.Fprintf(conn, "AUTH\t1\tPLAIN\tservice=imap\tresp=alice\x00alice\x00secret\n")

	if !sc.Scan() {
		t.Fatalf("no auth response line: %v", sc.Err())
	}
	resp := sc.Text()
	if !strings.HasPrefix(resp, "OK\t1\t") {
		t.Errorf("expected OK response, got: %q", resp)
	}
	if !strings.Contains(resp, "user=alice") {
		t.Errorf("expected user=alice in response, got: %q", resp)
	}
}

func TestListenAndServe_AuthFail(t *testing.T) {
	srv := NewServer([]Passdb{&credPassdb{"alice", "secret"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))

	sc := bufio.NewScanner(conn)
	readHandshake(t, sc)

	fmt.Fprintf(conn, "AUTH\t2\tPLAIN\tservice=imap\tresp=alice\x00alice\x00wrongpass\n")

	if !sc.Scan() {
		t.Fatalf("no auth response line: %v", sc.Err())
	}
	resp := sc.Text()
	if !strings.HasPrefix(resp, "FAIL\t2") {
		t.Errorf("expected FAIL response, got: %q", resp)
	}
}

func TestListenAndServe_GracefulShutdown(t *testing.T) {
	srv := NewServer(nil)
	ctx, cancel := context.WithCancel(context.Background())

	addr := freeAddr(t)
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx, addr, nil) }()
	time.Sleep(20 * time.Millisecond)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ListenAndServe returned error after cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("ListenAndServe did not return after context cancel")
	}
}
