// Package director implements the yarilo-director TCP+mTLS routing server.
// Login pods call it to resolve which backend pod should handle a given user,
// and health pods call it to register/deregister backends.
//
// Protocol (TAB-delimited, LF-terminated):
//
//	Server → Client handshake:
//	  VERSION\tyarilo-director\t1\t0\n
//	  DONE\n
//
//	Client → Server handshake:
//	  VERSION\tyarilo-director\t1\t0\n
//	  ME\t{ip}\t{port}\t{ts}\n
//	  DONE\n
//
//	Client commands:
//	  LOOKUP\t{id}\t{user}\n
//	  BACKEND-UP\t{ip}\t{port}\t{tag}\n
//	  BACKEND-DOWN\t{ip}\n
//
//	Server responses:
//	  HOST\t{id}\t{ip}\t{port}\n
//	  FAIL\t{id}\treason=no-backends\n
//	  OK\n
package director

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/0kaba0hub/yarilo/internal/cluster/ring"
)

const (
	protoName = "yarilo-director"
	majorVer  = 1
	minorVer  = 0
)

// Server is the yarilo-director TCP server. It wraps a ring.Ring and exposes
// it over the wire protocol so login and health pods can share ring state.
type Server struct {
	ring *ring.Ring
}

// New creates a director server with an empty ring.
func New() *Server {
	return &Server{ring: ring.New()}
}

// ListenAndServe starts the director TCP server. When tlsCfg is non-nil the
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
		return fmt.Errorf("director: listen %s: %w", addr, err)
	}
	return s.listenOn(ctx, ln)
}

func (s *Server) listenOn(ctx context.Context, ln net.Listener) error {
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
			return fmt.Errorf("director: accept: %w", err)
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

	// Read client handshake — consume until DONE.
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\n")
		if line == "DONE" {
			break
		}
		fields := strings.Split(line, "\t")
		if len(fields) >= 4 && fields[0] == "ME" {
			slog.Debug("director: client identified", "ip", fields[1], "port", fields[2])
		}
	}

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
		case "LOOKUP":
			s.handleLookup(conn, fields)
		case "BACKEND-UP":
			s.handleBackendUp(conn, fields)
		case "BACKEND-DOWN":
			s.handleBackendDown(conn, fields)
		}
	}
}

// handleLookup processes: LOOKUP\t{id}\t{user}
func (s *Server) handleLookup(conn net.Conn, fields []string) {
	if len(fields) < 3 {
		return
	}
	id, user := fields[1], fields[2]
	b := s.ring.LookupBackend(user)
	if b == nil {
		fmt.Fprintf(conn, "FAIL\t%s\treason=no-backends\n", id)
		return
	}
	fmt.Fprintf(conn, "HOST\t%s\t%s\t%d\n", id, b.IP, b.Port)
}

// handleBackendUp processes: BACKEND-UP\t{ip}\t{port}\t{tag}
func (s *Server) handleBackendUp(conn net.Conn, fields []string) {
	if len(fields) < 3 {
		fmt.Fprintf(conn, "OK\n")
		return
	}
	ip := fields[1]
	port, err := strconv.Atoi(fields[2])
	if err != nil {
		fmt.Fprintf(conn, "OK\n")
		return
	}
	tag := ""
	if len(fields) >= 4 {
		tag = fields[3]
	}
	s.ring.AddBackend(&ring.Backend{IP: ip, Port: port, Tag: tag, Up: true})
	slog.Info("director: backend up", "ip", ip, "port", port, "tag", tag)
	fmt.Fprintf(conn, "OK\n")
}

// handleBackendDown processes: BACKEND-DOWN\t{ip}
func (s *Server) handleBackendDown(conn net.Conn, fields []string) {
	if len(fields) < 2 {
		fmt.Fprintf(conn, "OK\n")
		return
	}
	ip := fields[1]
	s.ring.RemoveBackend(ip)
	slog.Info("director: backend down", "ip", ip)
	fmt.Fprintf(conn, "OK\n")
}
