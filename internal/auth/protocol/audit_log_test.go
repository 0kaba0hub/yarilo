package protocol

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureSlog swaps the default slog logger to a buffer for the
// duration of the test and returns the buffer + a restore func.
// syncBuffer is a bytes.Buffer safe for concurrent Write and String. The server
// logs from its own goroutines (and, since #887, from one goroutine per command),
// while the test reads the buffer from the test goroutine — a bare bytes.Buffer
// races there.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureSlog(t *testing.T) (*syncBuffer, func()) {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	return buf, func() { slog.SetDefault(prev) }
}

// TestWire_Audit_RegularLoginLogsEmptyMaster — every successful
// login produces `auth: ok` with master_user="".
func TestWire_Audit_RegularLoginLogsEmptyMaster(t *testing.T) {
	buf, restore := captureSlog(t)
	defer restore()

	srv := NewServer([]Passdb{&credPassdb{"alice", "secret"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()

	fmt.Fprintf(conn, "AUTH\t70\tPLAIN\tservice=imap\tresp=\x00alice\x00secret\n")
	if !sc.Scan() {
		t.Fatalf("no reply: %v", sc.Err())
	}
	// Settle so the slog line lands.
	time.Sleep(20 * time.Millisecond)

	out := buf.String()
	if !strings.Contains(out, `msg="auth: ok"`) {
		t.Errorf("missing auth: ok log line: %s", out)
	}
	if !strings.Contains(out, `user=alice`) {
		t.Errorf("missing user=alice: %s", out)
	}
	if !strings.Contains(out, `master_user=""`) {
		t.Errorf("regular login should log master_user=\"\": %s", out)
	}
}

// TestWire_Audit_MasterFlowLogsMaster — master-user impersonation
// logs `auth: ok` with master_user=<admin>.
func TestWire_Audit_MasterFlowLogsMaster(t *testing.T) {
	buf, restore := captureSlog(t)
	defer restore()

	srv := NewServer(
		[]Passdb{&credPassdb{"alice", "userpass"}},
		WithMasterUsers(true),
		WithMasterdb([]Passdb{&credPassdb{"admin", "masterpass"}}),
		WithUserdb(targetUserdbForWire{}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()

	fmt.Fprintf(conn, "AUTH\t71\tPLAIN\tservice=imap\tresp=alice\x00admin\x00masterpass\n")
	if !sc.Scan() {
		t.Fatalf("no reply: %v", sc.Err())
	}
	time.Sleep(20 * time.Millisecond)

	out := buf.String()
	if !strings.Contains(out, `master_user=admin`) {
		t.Errorf("master flow missing master_user=admin: %s", out)
	}
	if !strings.Contains(out, `user=alice`) {
		t.Errorf("master flow should log target user=alice: %s", out)
	}
}

// TestWire_Audit_FailLogs — failed credentials produce
// `auth: fail` log with the attempted username so SIEM can
// correlate failures by user.
func TestWire_Audit_FailLogs(t *testing.T) {
	buf, restore := captureSlog(t)
	defer restore()

	srv := NewServer([]Passdb{&credPassdb{"alice", "secret"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()

	fmt.Fprintf(conn, "AUTH\t72\tPLAIN\tservice=imap\tresp=\x00alice\x00WRONG\n")
	if !sc.Scan() {
		t.Fatalf("no reply: %v", sc.Err())
	}
	time.Sleep(20 * time.Millisecond)

	out := buf.String()
	if !strings.Contains(out, `msg="auth: fail"`) {
		t.Errorf("missing auth: fail log: %s", out)
	}
	if !strings.Contains(out, `user=alice`) {
		t.Errorf("missing user=alice in fail log: %s", out)
	}
}
