// Package anvil implements the yarilo-anvil TCP+mTLS connection-accounting server.
// Login pods call it to enforce mail_max_userip_connections across the cluster.
//
// Protocol (TAB-delimited, LF-terminated):
//
//	Server → Client handshake:
//	  VERSION\tyarilo-anvil\t1\t0\n
//	  DONE\n
//
//	Client commands:
//	  CONNECT\t{id}\t{user}\t{ip}\t{service}\n
//	  DISCONNECT\t{id}\t{user}\t{ip}\t{service}\n
//
//	Server responses:
//	  OK\t{id}\n
//	  FAIL\t{id}\treason=too-many-connections\n
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

	"github.com/0kaba0hub/yarilo/internal/connlimit"
)

const (
	protoName = "yarilo-anvil"
	majorVer  = 1
	minorVer  = 0
)

// Server is the yarilo-anvil TCP server. It wraps a connlimit.Limiter and
// exposes it over the wire protocol so multiple login pods can share state.
type Server struct {
	limiter *connlimit.Limiter
}

// NewServer creates an anvil server with the given per-user@IP connection limit.
// max ≤ 0 means unlimited (server still runs but always returns OK).
func NewServer(max int) *Server {
	return &Server{limiter: connlimit.New(max)}
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
		}
	}
}

// handleConnect processes: CONNECT\t{id}\t{user}\t{ip}\t{service}
func (s *Server) handleConnect(conn net.Conn, fields []string) {
	if len(fields) < 4 {
		return
	}
	id, user, ip := fields[1], fields[2], fields[3]
	if !s.limiter.Acquire(user, ip) {
		fmt.Fprintf(conn, "FAIL\t%s\treason=too-many-connections\n", id)
		return
	}
	fmt.Fprintf(conn, "OK\t%s\n", id)
}

// handleDisconnect processes: DISCONNECT\t{id}\t{user}\t{ip}\t{service}
func (s *Server) handleDisconnect(conn net.Conn, fields []string) {
	if len(fields) < 4 {
		return
	}
	id, user, ip := fields[1], fields[2], fields[3]
	s.limiter.Release(user, ip)
	fmt.Fprintf(conn, "OK\t%s\n", id)
}
