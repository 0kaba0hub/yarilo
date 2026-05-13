package smtp

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-milter"
)

// startFakeMilter starts a milter server that uses the provided Milter impl.
// Returns the TCP address and a close function.
func startFakeMilter(t *testing.T, m milter.Milter) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &milter.Server{
		NewMilter: func() milter.Milter { return m },
		Actions:   milter.OptAddHeader | milter.OptChangeHeader | milter.OptQuarantine,
		Protocol:  milter.OptNoConnect | milter.OptNoHelo,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() }) //nolint:errcheck
	return ln.Addr().String()
}

// acceptMilter accepts everything.
type acceptMilter struct{ milter.NoOpMilter }

// rejectAtMailFrom rejects at MAIL FROM.
type rejectAtMailFrom struct{ milter.NoOpMilter }

func (rejectAtMailFrom) MailFrom(_ string, _ *milter.Modifier) (milter.Response, error) {
	return milter.RespReject, nil
}

// rejectAtBody rejects at end-of-message.
type rejectAtBody struct{ milter.NoOpMilter }

func (rejectAtBody) Body(_ *milter.Modifier) (milter.Response, error) {
	return milter.RespReject, nil
}

// tempFailMilter returns TempFail at MAIL FROM.
type tempFailMilter struct{ milter.NoOpMilter }

func (tempFailMilter) MailFrom(_ string, _ *milter.Modifier) (milter.Response, error) {
	return milter.RespTempFail, nil
}

const milterMsg = "From: a@example.com\r\nTo: b@example.com\r\nSubject: test\r\n\r\nHello\r\n"

func TestMilterClient_Accept(t *testing.T) {
	addr := startFakeMilter(t, acceptMilter{})
	c, err := NewMilterClient("tcp:"+addr, 5)
	if err != nil {
		t.Fatalf("NewMilterClient: %v", err)
	}
	defer c.Close() //nolint:errcheck

	if err := c.Check(context.Background(), "a@example.com", []string{"b@example.com"}, strings.NewReader(milterMsg)); err != nil {
		t.Errorf("expected accept, got: %v", err)
	}
}

func TestMilterClient_RejectAtMailFrom(t *testing.T) {
	addr := startFakeMilter(t, rejectAtMailFrom{})
	c, err := NewMilterClient("tcp:"+addr, 5)
	if err != nil {
		t.Fatalf("NewMilterClient: %v", err)
	}
	defer c.Close() //nolint:errcheck

	err = c.Check(context.Background(), "a@example.com", []string{"b@example.com"}, strings.NewReader(milterMsg))
	if err == nil {
		t.Fatal("expected rejection, got nil")
	}
}

func TestMilterClient_RejectAtBody(t *testing.T) {
	addr := startFakeMilter(t, rejectAtBody{})
	c, err := NewMilterClient("tcp:"+addr, 5)
	if err != nil {
		t.Fatalf("NewMilterClient: %v", err)
	}
	defer c.Close() //nolint:errcheck

	err = c.Check(context.Background(), "a@example.com", []string{"b@example.com"}, strings.NewReader(milterMsg))
	if err == nil {
		t.Fatal("expected rejection, got nil")
	}
}

func TestMilterClient_TempFail(t *testing.T) {
	addr := startFakeMilter(t, tempFailMilter{})
	c, err := NewMilterClient("tcp:"+addr, 5)
	if err != nil {
		t.Fatalf("NewMilterClient: %v", err)
	}
	defer c.Close() //nolint:errcheck

	err = c.Check(context.Background(), "a@example.com", []string{"b@example.com"}, strings.NewReader(milterMsg))
	if err == nil {
		t.Fatal("expected tempfail rejection, got nil")
	}
}

func TestMilterClient_FailOpen(t *testing.T) {
	// Connect to a port with nothing listening — should fail-open (nil error).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // close immediately so connection will be refused

	c, err := NewMilterClient("tcp:"+addr, 1)
	if err != nil {
		t.Fatalf("NewMilterClient: %v", err)
	}
	defer c.Close() //nolint:errcheck

	// Milter unavailable → fail-open → nil error.
	if err := c.Check(context.Background(), "a@example.com", []string{"b@example.com"}, strings.NewReader(milterMsg)); err != nil {
		t.Errorf("fail-open: expected nil, got: %v", err)
	}
}
