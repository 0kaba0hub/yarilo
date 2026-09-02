package locks

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"
)

// Server is the wire-level locks server. It is transport-agnostic — call
// Serve with any net.Listener (Unix socket for embedded, TLS-wrapped TCP
// for remote). All state lives in the supplied Backend.
type Server struct {
	backend Backend
	logger  *slog.Logger
	metrics *Metrics

	closing chan struct{}
	closeMu sync.Mutex
	closed  bool

	wg sync.WaitGroup
}

// NewServer constructs a Server around a Backend. The Backend's lifecycle is
// owned by the caller — Server.Close does not call backend.Close.
//
// A nil metrics argument means "no metrics" — the Metrics methods are
// nil-safe. Tests and benchmarks that spin up many servers should pass nil
// to avoid duplicate-registration panics on prometheus.DefaultRegisterer.
func NewServer(backend Backend, logger *slog.Logger, metrics *Metrics) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		backend: backend,
		logger:  logger,
		metrics: metrics,
		closing: make(chan struct{}),
	}
}

// Serve accepts connections on ln until ctx is cancelled or ln.Accept errors
// terminally. ln is closed by Serve before returning.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	defer func() { _ = ln.Close() }()

	go func() {
		select {
		case <-ctx.Done():
		case <-s.closing:
		}
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				s.wg.Wait()
				return nil
			}
			s.logger.Error("locks: accept failed", "err", err)
			return fmt.Errorf("locks/server: accept: %w", err)
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(ctx, conn)
		}()
	}
}

// Close stops accepting new connections and waits for in-flight handlers to
// drain. Idempotent.
func (s *Server) Close() {
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return
	}
	s.closed = true
	close(s.closing)
	s.closeMu.Unlock()
	s.wg.Wait()
}

// handleConn serves a single connection.
//
// Protocol: handshake (VERSION exchange) → command loop. A connection that
// invokes SUBSCRIBE leaves the command loop and streams events until the
// peer closes the conn or its subscription context is cancelled.
func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	peer := conn.RemoteAddr().String()
	r := newReader(conn)

	if err := s.handshake(r, conn); err != nil {
		s.logger.Warn("locks: handshake failed", "peer", peer, "err", err)
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.closing:
			return
		default:
		}

		fields, err := r.readFields()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.logger.Debug("locks: read failed", "peer", peer, "err", err)
			}
			return
		}
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case cmdLock:
			s.handleLock(ctx, conn, fields, peer)
		case cmdLockShared:
			s.handleLockShared(ctx, conn, fields, peer)
		case cmdUnlock:
			s.handleUnlock(ctx, conn, fields, peer)
		case cmdRenew:
			s.handleRenew(ctx, conn, fields, peer)
		case cmdEmit:
			s.handleEmit(ctx, conn, fields, peer)
		case cmdSubscribe:
			s.handleSubscribe(ctx, conn, fields, peer)
			return // subscribe takes over the conn
		case cmdCounterInc:
			s.handleCounterInc(ctx, conn, fields, peer)
		default:
			_ = writeFields(conn, respError, "unknown_command")
		}
	}
}

func (s *Server) handshake(r *reader, w io.Writer) error {
	fields, err := r.readFields()
	if err != nil {
		return fmt.Errorf("locks/server: read version: %w", err)
	}
	if len(fields) != 2 || fields[0] != cmdVersion || fields[1] != protocolVersion {
		_ = writeFields(w, respError, "version_mismatch")
		return fmt.Errorf("locks/server: unexpected version frame %v: %w", fields, ErrProtocol)
	}
	return writeFields(w, cmdVersion, protocolVersion, respOK)
}

func (s *Server) handleLock(ctx context.Context, w io.Writer, fields []string, peer string) {
	if len(fields) != 4 {
		_ = writeFields(w, respError, "bad_lock")
		return
	}
	resource, owner := fields[1], fields[2]
	ttl, err := parseTTL(fields[3])
	if err != nil {
		_ = writeFields(w, respError, "bad_ttl")
		return
	}
	s.metrics.observeContender(resource, owner)
	start := time.Now()
	id, current, err := s.backend.Acquire(ctx, resource, owner, ttl)
	dur := time.Since(start).Seconds()
	switch {
	case err == nil:
		s.metrics.observeAcquire(dur, "ok")
		s.logger.Debug("locks: acquired", "peer", peer, "resource", resource, "owner", owner, "id", id, "dur_ms", dur*1000)
		_ = writeFields(w, respOK, id)
	case errors.Is(err, ErrBusy):
		s.metrics.observeAcquire(dur, "busy")
		s.metrics.incBusy()
		s.logger.Debug("locks: busy", "peer", peer, "resource", resource, "owner", owner, "held_by", current)
		_ = writeFields(w, respBusy, current)
	default:
		s.metrics.observeAcquire(dur, "error")
		s.logger.Error("locks: acquire failed", "peer", peer, "resource", resource, "err", err)
		_ = writeFields(w, respError, "internal")
	}
}

func (s *Server) handleLockShared(ctx context.Context, w io.Writer, fields []string, peer string) {
	if len(fields) != 4 {
		_ = writeFields(w, respError, "bad_lock")
		return
	}
	resource, owner := fields[1], fields[2]
	ttl, err := parseTTL(fields[3])
	if err != nil {
		_ = writeFields(w, respError, "bad_ttl")
		return
	}
	s.metrics.observeContender(resource, owner)
	start := time.Now()
	id, current, err := s.backend.AcquireShared(ctx, resource, owner, ttl)
	dur := time.Since(start).Seconds()
	switch {
	case err == nil:
		s.metrics.observeAcquire(dur, "ok")
		s.logger.Debug("locks: acquired shared", "peer", peer, "resource", resource, "owner", owner, "id", id, "dur_ms", dur*1000)
		_ = writeFields(w, respOK, id)
	case errors.Is(err, ErrBusy):
		s.metrics.observeAcquire(dur, "busy")
		s.metrics.incBusy()
		s.logger.Debug("locks: busy (shared)", "peer", peer, "resource", resource, "owner", owner, "held_by", current)
		_ = writeFields(w, respBusy, current)
	default:
		s.metrics.observeAcquire(dur, "error")
		s.logger.Error("locks: acquire shared failed", "peer", peer, "resource", resource, "err", err)
		_ = writeFields(w, respError, "internal")
	}
}

func (s *Server) handleUnlock(ctx context.Context, w io.Writer, fields []string, peer string) {
	if len(fields) != 2 {
		_ = writeFields(w, respError, "bad_unlock")
		return
	}
	lockID := fields[1]
	if err := s.backend.Release(ctx, lockID); err != nil {
		if errors.Is(err, ErrNotFound) {
			_ = writeFields(w, respNotFound)
			return
		}
		s.logger.Error("locks: release failed", "peer", peer, "err", err)
		_ = writeFields(w, respError, "internal")
		return
	}
	s.logger.Debug("locks: released", "peer", peer, "id", lockID)
	_ = writeFields(w, respOK)
}

func (s *Server) handleRenew(ctx context.Context, w io.Writer, fields []string, peer string) {
	if len(fields) != 3 {
		_ = writeFields(w, respError, "bad_renew")
		return
	}
	ttl, err := parseTTL(fields[2])
	if err != nil {
		_ = writeFields(w, respError, "bad_ttl")
		return
	}
	lockID := fields[1]
	if err := s.backend.Renew(ctx, lockID, ttl); err != nil {
		if errors.Is(err, ErrExpired) {
			s.metrics.incRenewFailed()
			_ = writeFields(w, respExpired)
			return
		}
		s.logger.Error("locks: renew failed", "peer", peer, "err", err)
		_ = writeFields(w, respError, "internal")
		return
	}
	s.logger.Debug("locks: renewed", "peer", peer, "id", lockID)
	_ = writeFields(w, respOK)
}

// handleCounterInc processes COUNTER-INC\t<key>\t<delta>\n and
// replies OK\t<new_value>\n. delta may be negative (callers can
// reset a counter via two INCRs around its current value); zero
// deltas are still valid and act as a no-op read.
func (s *Server) handleCounterInc(ctx context.Context, w io.Writer, fields []string, peer string) {
	if len(fields) != 3 {
		_ = writeFields(w, respError, "bad_counter_inc")
		return
	}
	delta, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		_ = writeFields(w, respError, "bad_delta")
		return
	}
	v, err := s.backend.IncrementCounter(ctx, fields[1], delta)
	if err != nil {
		s.logger.Error("locks: counter inc failed", "peer", peer, "key", fields[1], "err", err)
		_ = writeFields(w, respError, "internal")
		return
	}
	_ = writeFields(w, respOK, strconv.FormatInt(v, 10))
}

func (s *Server) handleEmit(ctx context.Context, w io.Writer, fields []string, peer string) {
	if len(fields) != 4 {
		_ = writeFields(w, respError, "bad_emit")
		return
	}
	if err := s.backend.Publish(ctx, fields[1], EventType(fields[2]), fields[3]); err != nil {
		s.logger.Error("locks: publish failed", "peer", peer, "err", err)
		_ = writeFields(w, respError, "internal")
		return
	}
	_ = writeFields(w, respOK)
}

func (s *Server) handleSubscribe(ctx context.Context, conn net.Conn, fields []string, peer string) {
	if len(fields) != 2 {
		_ = writeFields(conn, respError, "bad_subscribe")
		return
	}
	resource := fields[1]
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, release, err := s.backend.Subscribe(subCtx, resource)
	if err != nil {
		s.logger.Error("locks: subscribe failed", "peer", peer, "resource", resource, "err", err)
		_ = writeFields(conn, respError, "internal")
		return
	}
	defer release()
	if err := writeFields(conn, respOK); err != nil {
		return
	}
	// Detect peer close so we exit even with no events flowing.
	go func() {
		buf := make([]byte, 1)
		_, _ = conn.Read(buf)
		cancel()
	}()
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				return
			}
			if err := writeFields(conn, respEvent, evt.Resource, string(evt.Type), evt.Payload); err != nil {
				return
			}
		case <-subCtx.Done():
			return
		case <-s.closing:
			return
		}
	}
}

// ListenUnix opens a Unix socket listener. Caller is responsible for removing
// any stale socket at path before calling.
func ListenUnix(path string) (net.Listener, error) {
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("locks/server: listen unix %q: %w", path, err)
	}
	return ln, nil
}

// ListenTLS opens a TCP listener wrapped in mTLS using the supplied config.
func ListenTLS(addr string, tlsCfg *tls.Config) (net.Listener, error) {
	if tlsCfg == nil {
		return nil, fmt.Errorf("locks/server: nil tls config for remote mode")
	}
	ln, err := tls.Listen("tcp", addr, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("locks/server: listen tls %q: %w", addr, err)
	}
	return ln, nil
}
