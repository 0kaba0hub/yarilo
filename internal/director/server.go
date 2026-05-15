// Package director implements the yarilo-director TCP+mTLS routing server.
// Login pods call it to resolve which backend pod should handle a given user,
// and health pods call it to register/deregister backends from the hash ring.
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
//	  BACKEND-UP\t{ip}\t{port}\t{tag}\t{vhosts}\n   (vhosts optional; 0 = 100)
//	  BACKEND-DOWN\t{ip}\n
//	  BACKEND-FLUSH\t{ip}\n                          (drain: stop new lookups, keep backend in registry)
//	  USER-MOVE\t{user}\t{ip}\t{port}\n              (force user to specific backend)
//	  USER-RELEASE\t{user}\n                         (remove user override)
//
//	Server responses:
//	  HOST\t{id}\t{ip}\t{port}\n
//	  FAIL\t{id}\treason=no-backends\n
//	  OK\n
//
//	Server pushes (unsolicited, to all connected clients):
//	  RING-CHANGE\t{ip}\t{event}\n                  (event: up | down | flush)
//	  USER-MOVED\t{user}\t{ip}\t{port}\n             (when user is moved by another client)
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

// client wraps an active connection with a per-connection write lock so
// unsolicited pushes never interleave with command responses.
type client struct {
	conn net.Conn
	mu   sync.Mutex
}

func (c *client) WriteLine(line string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := io.WriteString(c.conn, line+"\n")
	return err
}

// Server is the yarilo-director TCP server.
type Server struct {
	ring *ring.Ring

	// userOverrides maps username → "ip:port" for admin-forced assignments.
	// Set via USER-MOVE, cleared via USER-RELEASE.
	overrideMu sync.RWMutex
	overrides  map[string]string

	// clients is the registry of all currently connected clients.
	// Used to broadcast RING-CHANGE and USER-MOVED events.
	clientMu sync.RWMutex
	clients  map[*client]struct{}
}

// New creates a director server with an empty ring.
func New() *Server {
	return &Server{
		ring:      ring.New(),
		overrides: make(map[string]string),
		clients:   make(map[*client]struct{}),
	}
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

func (s *Server) addClient(c *client) {
	s.clientMu.Lock()
	s.clients[c] = struct{}{}
	s.clientMu.Unlock()
}

func (s *Server) removeClient(c *client) {
	s.clientMu.Lock()
	delete(s.clients, c)
	s.clientMu.Unlock()
}

// broadcast sends an unsolicited line to all connected clients except the one
// that triggered the change. Errors are silently ignored — a dead client will
// be removed when its read loop exits.
func (s *Server) broadcast(line string, exclude *client) {
	s.clientMu.RLock()
	defer s.clientMu.RUnlock()
	for c := range s.clients {
		if c == exclude {
			continue
		}
		_ = c.WriteLine(line)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	c := &client{conn: conn}
	s.addClient(c)
	defer s.removeClient(c)

	rd := bufio.NewReaderSize(conn, 4096)

	_ = c.WriteLine(fmt.Sprintf("VERSION\t%s\t%d\t%d", protoName, majorVer, minorVer))
	_ = c.WriteLine("DONE")

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
		if len(fields) >= 3 && fields[0] == "ME" {
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
			s.handleLookup(c, fields)
		case "BACKEND-UP":
			s.handleBackendUp(c, fields)
		case "BACKEND-DOWN":
			s.handleBackendDown(c, fields)
		case "BACKEND-FLUSH":
			s.handleBackendFlush(c, fields)
		case "USER-MOVE":
			s.handleUserMove(c, fields)
		case "USER-RELEASE":
			s.handleUserRelease(c, fields)
		}
	}
}

// handleLookup processes: LOOKUP\t{id}\t{user}
// Checks admin overrides first, then falls back to ring consistent hashing.
// Response: HOST\t{id}\t{ip}\t{port}\t{tag}
func (s *Server) handleLookup(c *client, fields []string) {
	if len(fields) < 3 {
		return
	}
	id, user := fields[1], fields[2]

	// Check admin override — look up the backend entry to get the tag.
	s.overrideMu.RLock()
	addr, hasOverride := s.overrides[user]
	s.overrideMu.RUnlock()

	if hasOverride {
		host, portStr, err := net.SplitHostPort(addr)
		if err == nil {
			// Best-effort tag lookup from ring (override may point to a non-ring backend).
			tag := ""
			for _, b := range s.ring.Backends() {
				if b.IP == host {
					tag = b.Tag
					break
				}
			}
			_ = c.WriteLine(fmt.Sprintf("HOST\t%s\t%s\t%s\t%s", id, host, portStr, tag))
			return
		}
	}

	// Ring lookup.
	b := s.ring.LookupBackend(user)
	if b == nil {
		_ = c.WriteLine(fmt.Sprintf("FAIL\t%s\treason=no-backends", id))
		return
	}
	_ = c.WriteLine(fmt.Sprintf("HOST\t%s\t%s\t%d\t%s", id, b.IP, b.Port, b.Tag))
}

// handleBackendUp processes: BACKEND-UP\t{ip}\t{port}\t{tag}\t{vhosts}
// vhosts is optional; 0 or absent means default (100).
func (s *Server) handleBackendUp(c *client, fields []string) {
	if len(fields) < 3 {
		_ = c.WriteLine("OK")
		return
	}
	ip := fields[1]
	port, err := strconv.Atoi(fields[2])
	if err != nil {
		_ = c.WriteLine("OK")
		return
	}
	tag := ""
	if len(fields) >= 4 {
		tag = fields[3]
	}
	vhosts := 0
	if len(fields) >= 5 {
		vhosts, _ = strconv.Atoi(fields[4])
	}
	s.ring.AddBackend(&ring.Backend{IP: ip, Port: port, Tag: tag, Up: true, Vhosts: vhosts})
	slog.Info("director: backend up", "ip", ip, "port", port, "tag", tag, "vhosts", vhosts)
	s.broadcast(fmt.Sprintf("RING-CHANGE\t%s\tup\t%s", ip, tag), c)
	_ = c.WriteLine("OK")
}

// handleBackendDown processes: BACKEND-DOWN\t{ip}
// Removes the backend from the ring entirely.
func (s *Server) handleBackendDown(c *client, fields []string) {
	if len(fields) < 2 {
		_ = c.WriteLine("OK")
		return
	}
	ip := fields[1]
	// Capture tag before removal for the broadcast.
	tag := ""
	for _, b := range s.ring.Backends() {
		if b.IP == ip {
			tag = b.Tag
			break
		}
	}
	s.ring.RemoveBackend(ip)
	slog.Info("director: backend down", "ip", ip)
	s.broadcast(fmt.Sprintf("RING-CHANGE\t%s\tdown\t%s", ip, tag), c)
	_ = c.WriteLine("OK")
}

// handleBackendFlush processes: BACKEND-FLUSH\t{ip}
// Marks the backend as !Up so no new lookups are routed there, but keeps it
// in the registry so its state is visible and it can be brought back up later.
func (s *Server) handleBackendFlush(c *client, fields []string) {
	if len(fields) < 2 {
		_ = c.WriteLine("OK")
		return
	}
	ip := fields[1]
	// Capture tag before marking down for the broadcast.
	tag := ""
	for _, b := range s.ring.Backends() {
		if b.IP == ip {
			tag = b.Tag
			break
		}
	}
	if !s.ring.SetUp(ip, false) {
		slog.Warn("director: flush requested for unknown backend", "ip", ip)
		_ = c.WriteLine("OK")
		return
	}
	slog.Info("director: backend flush", "ip", ip)
	s.broadcast(fmt.Sprintf("RING-CHANGE\t%s\tflush\t%s", ip, tag), c)
	_ = c.WriteLine("OK")
}

// handleUserMove processes: USER-MOVE\t{user}\t{ip}\t{port}
// Forces the user to be routed to a specific backend, overriding the ring.
func (s *Server) handleUserMove(c *client, fields []string) {
	if len(fields) < 4 {
		_ = c.WriteLine("OK")
		return
	}
	user, ip, portStr := fields[1], fields[2], fields[3]
	addr := net.JoinHostPort(ip, portStr)

	s.overrideMu.Lock()
	s.overrides[user] = addr
	s.overrideMu.Unlock()

	slog.Info("director: user moved", "user", user, "backend", addr)
	s.broadcast(fmt.Sprintf("USER-MOVED\t%s\t%s\t%s", user, ip, portStr), c)
	_ = c.WriteLine("OK")
}

// handleUserRelease processes: USER-RELEASE\t{user}
// Removes the admin override so the user falls back to ring-based routing.
func (s *Server) handleUserRelease(c *client, fields []string) {
	if len(fields) < 2 {
		_ = c.WriteLine("OK")
		return
	}
	user := fields[1]

	s.overrideMu.Lock()
	delete(s.overrides, user)
	s.overrideMu.Unlock()

	slog.Info("director: user released", "user", user)
	_ = c.WriteLine("OK")
}
