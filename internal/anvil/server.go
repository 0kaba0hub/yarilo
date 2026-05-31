// Package anvil implements the yarilo-anvil TCP+mTLS connection-accounting server.
// Login pods call it to enforce mail_max_userip_connections across the cluster.
//
// Protocol (TAB-delimited, LF-terminated):
//
//	Server → Client handshake:
//	  VERSION\tyarilo-anvil\t1\t1\n
//	  DONE\n
//
//	Client commands:
//	  CONNECT\t{id}\t{user}\t{ip}\t{service}\n
//	  DISCONNECT\t{id}\t{user}\t{ip}\t{service}\n
//	  WHO[\tservice={s}][\tuser={u}]\n
//
//	Server responses:
//	  OK\t{id}\n
//	  FAIL\t{id}\treason=too-many-connections\n
//	  SESSION\t{id}\t{user}\t{ip}\t{service}\t{connect_unix}\n
//	  DONE\n
//
// WHO replies with one SESSION line per matching active session,
// then DONE. The optional service= / user= tokens narrow the list.
// Minor protocol version bumped 1.0 → 1.1 when WHO landed; older
// clients ignore the unknown command entirely.
package anvil

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/0kaba0hub/yarilo/internal/connlimit"
)

const (
	protoName = "yarilo-anvil"
	majorVer  = 1
	minorVer  = 1
)

// SessionInfo is one tracked client connection. Exported so the
// client package can return parsed WHO rows directly.
type SessionInfo struct {
	ID          string
	User        string
	IP          string
	Service     string
	ConnectedAt time.Time
}

// Server is the yarilo-anvil TCP server. It wraps a connlimit.Limiter and
// exposes it over the wire protocol so multiple login pods can share state.
type Server struct {
	limiter *connlimit.Limiter

	mu       sync.Mutex
	sessions map[string]*SessionInfo // id → session
}

// NewServer creates an anvil server with the given per-user@IP connection limit.
// max ≤ 0 means unlimited (server still runs but always returns OK).
func NewServer(max int) *Server {
	return &Server{
		limiter:  connlimit.New(max),
		sessions: make(map[string]*SessionInfo),
	}
}

// Sessions returns a snapshot of every tracked session. Used by
// tests and by the in-process accounting path (no wire detour).
func (s *Server) Sessions() []*SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*SessionInfo, 0, len(s.sessions))
	for _, sess := range s.sessions {
		clone := *sess
		out = append(out, &clone)
	}
	return out
}

// ListenAndServe starts the anvil TCP server. When tlsCfg is non-nil the
// listener uses mTLS. Blocks until ctx is cancelled; active sessions drain
// before the function returns.
func (s *Server) ListenAndServe(ctx context.Context, addr string, tlsCfg *tls.Config) error {
	var ln net.Listener
	var err error
	if tlsCfg != nil {
		ln, err = tls.Listen("tcp", addr, tlsCfg)
	} else {
		ln, err = net.Listen("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("anvil: listen %s: %w", addr, err)
	}

	var wg sync.WaitGroup
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			wg.Wait()
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("anvil: accept: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleConn(conn)
		}()
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	rd := bufio.NewReaderSize(conn, 4096)

	fmt.Fprintf(conn, "VERSION\t%s\t%d\t%d\n", protoName, majorVer, minorVer)
	fmt.Fprintf(conn, "DONE\n")

	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				_ = err
			}
			return
		}
		line = strings.TrimRight(line, "\n")
		fields := strings.Split(line, "\t")
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "CONNECT":
			s.handleConnect(conn, fields)
		case "DISCONNECT":
			s.handleDisconnect(conn, fields)
		case "WHO":
			s.handleWho(conn, fields[1:])
		}
	}
}

// handleConnect processes: CONNECT\t{id}\t{user}\t{ip}\t{service}
func (s *Server) handleConnect(conn net.Conn, fields []string) {
	if len(fields) < 5 {
		// Tolerate the legacy 4-field form (no service) for back-compat
		// with v1.0 clients — service stays empty in the session record.
		if len(fields) < 4 {
			return
		}
		fields = append(fields, "")
	}
	id, user, ip, service := fields[1], fields[2], fields[3], fields[4]
	if !s.limiter.Acquire(user, ip) {
		fmt.Fprintf(conn, "FAIL\t%s\treason=too-many-connections\n", id)
		return
	}
	s.mu.Lock()
	s.sessions[id] = &SessionInfo{
		ID:          id,
		User:        user,
		IP:          ip,
		Service:     service,
		ConnectedAt: time.Now().UTC(),
	}
	s.mu.Unlock()
	fmt.Fprintf(conn, "OK\t%s\n", id)
}

// handleDisconnect processes: DISCONNECT\t{id}\t{user}\t{ip}\t{service}
func (s *Server) handleDisconnect(conn net.Conn, fields []string) {
	if len(fields) < 4 {
		return
	}
	id, user, ip := fields[1], fields[2], fields[3]
	s.limiter.Release(user, ip)
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
	fmt.Fprintf(conn, "OK\t%s\n", id)
}

// handleWho processes: WHO[\tkey=value]...
//
// Recognised filter keys:
//
//	service={imap|pop3|submission|lmtp|...}
//	user={username}
//
// Unknown keys are ignored so future filters can land without
// breaking older anvil servers.
func (s *Server) handleWho(conn net.Conn, args []string) {
	filter := parseFilter(args)
	snap := s.Sessions()
	for _, sess := range snap {
		if v, ok := filter["service"]; ok && !strings.EqualFold(v, sess.Service) {
			continue
		}
		if v, ok := filter["user"]; ok && v != sess.User {
			continue
		}
		fmt.Fprintf(conn, "SESSION\t%s\t%s\t%s\t%s\t%d\n",
			sess.ID, sess.User, sess.IP, sess.Service, sess.ConnectedAt.Unix())
	}
	fmt.Fprintln(conn, "DONE")
}

func parseFilter(args []string) map[string]string {
	out := make(map[string]string, len(args))
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			continue
		}
		out[strings.ToLower(k)] = v
	}
	return out
}
