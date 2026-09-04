package backend

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/locks"
)

// shortSocketPath keeps Unix-socket paths under macOS/BSD sockaddr_un's 104-byte
// limit. t.TempDir() under /var/folders is too long.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "yl")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "l.sock")
}

// startEmbeddedServer spins an embedded yarilo-locks server bound to a Unix
// socket. Returns the socket path.
func startEmbeddedServer(t *testing.T) string {
	t.Helper()
	sock := shortSocketPath(t)
	backend := locks.NewMemoryBackend()
	srv := locks.NewServer(backend, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil)
	ln, err := locks.ListenUnix(sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(ctx, ln)
		close(done)
	}()
	// Wait for accept.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("unix", sock, 50*time.Millisecond)
		if err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() {
		srv.Close()
		cancel()
		_ = backend.Close()
		<-done
	})
	return sock
}

func TestBuildLocksClientDisabled(t *testing.T) {
	cfg := &config.Config{LocksClient: config.LocksClientConfig{Mode: ""}}
	lk, err := buildLocksClient(cfg)
	if err != nil {
		t.Fatalf("disabled: %v", err)
	}
	if lk != nil {
		t.Fatalf("disabled mode should return nil locker, got %T", lk)
	}
}

func TestBuildLocksClientEmbedded(t *testing.T) {
	sock := startEmbeddedServer(t)
	cfg := &config.Config{
		LocksClient: config.LocksClientConfig{Mode: "embedded", Socket: sock},
	}
	lk, err := buildLocksClient(cfg)
	if err != nil {
		t.Fatalf("embedded: %v", err)
	}
	defer func() { _ = lk.Close() }()
	// Sanity round-trip.
	lock, err := lk.Lock(context.Background(), "mbox:t:INBOX", "test.bin/1/alice@example.com/sess1", 5*time.Second)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	if err := lk.Unlock(context.Background(), lock.ID); err != nil {
		t.Fatalf("unlock: %v", err)
	}
}

func TestBuildLocksClientEmbeddedMissingSocket(t *testing.T) {
	cfg := &config.Config{LocksClient: config.LocksClientConfig{Mode: "embedded"}}
	_, err := buildLocksClient(cfg)
	if err == nil {
		t.Fatal("expected error for embedded mode without socket")
	}
}

func TestBuildLocksClientRemoteMissingEndpoints(t *testing.T) {
	cfg := &config.Config{LocksClient: config.LocksClientConfig{Mode: "remote"}}
	_, err := buildLocksClient(cfg)
	if err == nil {
		t.Fatal("expected error for remote mode without endpoints")
	}
}

func TestBuildLocksClientUnknownMode(t *testing.T) {
	cfg := &config.Config{LocksClient: config.LocksClientConfig{Mode: "nonsense"}}
	_, err := buildLocksClient(cfg)
	if err == nil {
		t.Fatal("expected error for unknown mode")
	}
}

func TestServerCloseIdempotent(t *testing.T) {
	sock := startEmbeddedServer(t)
	cfg := &config.Config{
		LocksClient: config.LocksClientConfig{Mode: "embedded", Socket: sock},
	}
	lk, err := buildLocksClient(cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	s := &Server{locker: lk}
	if err := s.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close 2 (idempotent): %v", err)
	}
}
