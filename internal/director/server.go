// Package director implements the yarilo-director TCP+mTLS routing server.
// Login pods call it to resolve which backend pod should handle a given user,
// and health pods call it to register/deregister backends from the hash ring.
//
// Protocol (TAB-delimited, LF-terminated):
//
//	Server → Client handshake:
//	  VERSION\tyarilo-director\t1\t0\n
//	  HOST-HAND-START\n
//	  HOST\t{ip}\t{port}\t{tag}\tD{down_ts}\tU{up_ts}\t{hostname}\n  (one per backend)
//	  HOST-HAND-END\n
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
//	  HOST-REMOVE\t{ip}\n                            (alias for BACKEND-DOWN)
//	  USER-MOVE\t{user}\t{ip}\t{port}\n              (force user to specific backend)
//	  USER-RELEASE\t{user}\n                         (remove user override)
//	  USER-WEAK\t{user}\n                            (mark current assignment as soft/weak)
//	  USER-KICK\t{user}\n                            (broadcast kick to all login clients)
//	  USER-KILLED\t{hash}\n                          (login reports sessions closed for hash)
//	  PING\n
//	  QUIT\t{reason}\n
//
//	Server responses:
//	  HOST\t{id}\t{ip}\t{port}\t{tag}\n
//	  FAIL\t{id}\treason=no-backends\n
//	  OK\n
//	  PONG\n
//
//	Server pushes (unsolicited, to all connected clients):
//	  RING-CHANGE\t{ip}\t{event}\t{tag}\n            (event: up | down | flush)
//	  USER-MOVED\t{user}\t{ip}\t{port}\n             (when user is moved by another client)
//	  USER-KICKED\t{user}\n                          (broadcast kick notification)
//	  USER-KILLED-EVERYWHERE\t{hash}\n               (director confirms all sessions gone)
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
	"time"

	"github.com/0kaba0hub/yarilo/internal/cluster/ring"
)

const (
	protoName = "yarilo-director"
	majorVer  = 1
	minorVer  = 0
)

// Options configures Server behaviour.
type Options struct {
	UserExpire   time.Duration // how long a user→backend mapping lives; default 900s
	PingInterval time.Duration // idle time before sending PING; default 30s
	PingTimeout  time.Duration // time to wait for PONG before closing; default 10s
}

func (o *Options) userExpire() time.Duration {
	if o.UserExpire <= 0 {
		return 900 * time.Second
	}
	return o.UserExpire
}

func (o *Options) pingInterval() time.Duration {
	if o.PingInterval <= 0 {
		return 30 * time.Second
	}
	return o.PingInterval
}

func (o *Options) pingTimeout() time.Duration {
	if o.PingTimeout <= 0 {
		return 10 * time.Second
	}
	return o.PingTimeout
}

// client wraps an active connection with a per-connection write lock so
// unsolicited pushes never interleave with command responses.
type client struct {
	conn   net.Conn
	mu     sync.Mutex
	pongCh chan struct{} // receives a token each time PONG is received
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
	opts Options

	// userDir stores user→backend mappings with TTL.
	userDir *UserDir

	// overrides maps username → "ip:port" for admin-forced assignments.
	overrideMu sync.RWMutex
	overrides  map[string]string

	// clients is the registry of all currently connected clients.
	clientMu sync.RWMutex
	clients  map[*client]struct{}
}

// New creates a director server with an empty ring and default options.
func New() *Server {
	return NewWithOptions(Options{})
}

// NewWithOptions creates a director server with custom options.
func NewWithOptions(opts Options) *Server {
	return &Server{
		ring:      ring.New(),
		opts:      opts,
		userDir:   NewUserDir(opts.userExpire()),
		overrides: make(map[string]string),
		clients:   make(map[*client]struct{}),
	}
}

// ListenAndServe starts the director TCP server. When tlsCfg is non-nil the
// listener uses mTLS. Blocks until ctx is cancelled.
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
// that triggered the change.
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

	c := &client{conn: conn, pongCh: make(chan struct{}, 4)}
	s.addClient(c)
	defer s.removeClient(c)

	rd := bufio.NewReaderSize(conn, 4096)

	// Send server handshake: VERSION + current ring state + DONE.
	_ = c.WriteLine(fmt.Sprintf("VERSION\t%s\t%d\t%d", protoName, majorVer, minorVer))
	_ = c.WriteLine("HOST-HAND-START")
	for _, b := range s.ring.Backends() {
		_ = c.WriteLine(hostLine(b))
	}
	_ = c.WriteLine("HOST-HAND-END")
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

	// Start PING/PONG keepalive goroutine.
	stopPing := make(chan struct{})
	go s.pingLoop(c, stopPing)
	defer close(stopPing)

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
		case "BACKEND-DOWN", "HOST-REMOVE":
			s.handleBackendDown(c, fields)
		case "BACKEND-FLUSH":
			s.handleBackendFlush(c, fields)
		case "USER-MOVE":
			s.handleUserMove(c, fields)
		case "USER-RELEASE":
			s.handleUserRelease(c, fields)
		case "USER-WEAK":
			s.handleUserWeak(c, fields)
		case "USER-KICK":
			s.handleUserKick(c, fields)
		case "USER-KILLED":
			s.handleUserKilled(c, fields)
		case "PONG":
			select {
			case c.pongCh <- struct{}{}:
			default:
			}
		case "PING":
			_ = c.WriteLine("PONG")
		case "QUIT":
			reason := ""
			if len(fields) >= 2 {
				reason = fields[1]
			}
			slog.Info("director: client quit", "reason", reason)
			return
		}
	}
}

// pingLoop sends periodic PING probes and closes the connection if PONG is not received.
func (s *Server) pingLoop(c *client, stop <-chan struct{}) {
	interval := s.opts.pingInterval()
	timeout := s.opts.pingTimeout()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if err := c.WriteLine("PING"); err != nil {
				return
			}
			// Wait for PONG within timeout.
			timer := time.NewTimer(timeout)
			select {
			case <-c.pongCh:
				timer.Stop()
			case <-timer.C:
				slog.Warn("director: PONG timeout, closing connection")
				c.conn.Close()
				return
			case <-stop:
				timer.Stop()
				return
			}
		}
	}
}

// hostLine formats a Backend as a HOST wire line for the handshake.
func hostLine(b ring.Backend) string {
	return fmt.Sprintf("HOST\t%s\t%d\t%s\tD%d\tU%d\t%s",
		b.IP, b.Port, b.Tag, b.LastUpdownChange, b.LastUpdownChange, b.Hostname)
}

// handleLookup processes: LOOKUP\t{id}\t{user}
// Checks admin overrides first, then user directory, then falls back to ring.
// Response: HOST\t{id}\t{ip}\t{port}\t{tag}
func (s *Server) handleLookup(c *client, fields []string) {
	if len(fields) < 3 {
		return
	}
	id, user := fields[1], fields[2]

	// Admin override wins everything.
	s.overrideMu.RLock()
	addr, hasOverride := s.overrides[user]
	s.overrideMu.RUnlock()

	if hasOverride {
		host, portStr, err := net.SplitHostPort(addr)
		if err == nil {
			tag := s.backendTag(host)
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

	// Record user→backend in directory.
	addr = net.JoinHostPort(b.IP, strconv.Itoa(b.Port))
	s.userDir.Set(user, addr, false)

	_ = c.WriteLine(fmt.Sprintf("HOST\t%s\t%s\t%d\t%s", id, b.IP, b.Port, b.Tag))
}

// handleBackendUp processes: BACKEND-UP\t{ip}\t{port}\t{tag}\t{vhosts}
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
	ts := time.Now().Unix()
	s.ring.AddBackend(&ring.Backend{
		IP: ip, Port: port, Tag: tag, Up: true, Vhosts: vhosts,
		LastUpdownChange: ts,
	})
	slog.Info("director: backend up", "ip", ip, "port", port, "tag", tag, "vhosts", vhosts)
	s.broadcast(fmt.Sprintf("RING-CHANGE\t%s\tup\t%s", ip, tag), c)
	_ = c.WriteLine("OK")
}

// handleBackendDown processes: BACKEND-DOWN\t{ip} and HOST-REMOVE\t{ip}
func (s *Server) handleBackendDown(c *client, fields []string) {
	if len(fields) < 2 {
		_ = c.WriteLine("OK")
		return
	}
	ip := fields[1]
	tag := s.backendTag(ip)
	s.ring.RemoveBackend(ip)
	slog.Info("director: backend down", "ip", ip)
	s.broadcast(fmt.Sprintf("RING-CHANGE\t%s\tdown\t%s", ip, tag), c)
	_ = c.WriteLine("OK")
}

// handleBackendFlush processes: BACKEND-FLUSH\t{ip}
// Marks backend !Up so no new lookups are routed there, keeps it in the registry.
func (s *Server) handleBackendFlush(c *client, fields []string) {
	if len(fields) < 2 {
		_ = c.WriteLine("OK")
		return
	}
	ip := fields[1]
	tag := s.backendTag(ip)
	if !s.ring.SetUp(ip, false, time.Now().Unix()) {
		slog.Warn("director: flush requested for unknown backend", "ip", ip)
		_ = c.WriteLine("OK")
		return
	}
	slog.Info("director: backend flush", "ip", ip)
	s.broadcast(fmt.Sprintf("RING-CHANGE\t%s\tflush\t%s", ip, tag), c)
	_ = c.WriteLine("OK")
}

// handleUserMove processes: USER-MOVE\t{user}\t{ip}\t{port}
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

	// Record in user directory as a strong assignment.
	s.userDir.Set(user, addr, false)

	slog.Info("director: user moved", "user", user, "backend", addr)
	s.broadcast(fmt.Sprintf("USER-MOVED\t%s\t%s\t%s", user, ip, portStr), c)
	_ = c.WriteLine("OK")
}

// handleUserRelease processes: USER-RELEASE\t{user}
func (s *Server) handleUserRelease(c *client, fields []string) {
	if len(fields) < 2 {
		_ = c.WriteLine("OK")
		return
	}
	user := fields[1]

	s.overrideMu.Lock()
	delete(s.overrides, user)
	s.overrideMu.Unlock()

	s.userDir.Delete(user)

	slog.Info("director: user released", "user", user)
	_ = c.WriteLine("OK")
}

// handleUserWeak processes: USER-WEAK\t{user}
// Marks the user's current directory entry as a soft/weak assignment.
// A weak assignment may be replaced if the user logs in from a different backend.
func (s *Server) handleUserWeak(c *client, fields []string) {
	if len(fields) < 2 {
		_ = c.WriteLine("OK")
		return
	}
	user := fields[1]
	e := s.userDir.Get(user)
	if e != nil {
		s.userDir.Set(user, e.Host, true)
	}
	slog.Info("director: user weakened", "user", user)
	_ = c.WriteLine("OK")
}

// handleUserKick processes: USER-KICK\t{user}
// Broadcasts a kick notification to all clients so login processes terminate
// existing sessions for this user.
func (s *Server) handleUserKick(c *client, fields []string) {
	if len(fields) < 2 {
		_ = c.WriteLine("OK")
		return
	}
	user := fields[1]
	slog.Info("director: user kick", "user", user)
	s.broadcast(fmt.Sprintf("USER-KICKED\t%s", user), c)
	_ = c.WriteLine("OK")
}

// handleUserKilled processes: USER-KILLED\t{hash}
// A login client reports that all sessions for this user hash have been terminated.
// The director broadcasts USER-KILLED-EVERYWHERE to confirm completion.
func (s *Server) handleUserKilled(c *client, fields []string) {
	if len(fields) < 2 {
		_ = c.WriteLine("OK")
		return
	}
	hashStr := fields[1]
	slog.Info("director: user killed", "hash", hashStr)
	s.broadcast(fmt.Sprintf("USER-KILLED-EVERYWHERE\t%s", hashStr), c)
	_ = c.WriteLine("OK")
}

// AddBackend registers a backend pod in the hash ring.
// Called at startup after DNS resolution of headless services.
func (s *Server) AddBackend(ip string, port int, tag string) {
	ts := time.Now().Unix()
	s.ring.AddBackend(&ring.Backend{
		IP:               ip,
		Port:             port,
		Tag:              tag,
		Up:               true,
		LastUpdownChange: ts,
	})
	slog.Info("director: backend registered", "ip", ip, "port", port, "tag", tag)
	s.broadcast(fmt.Sprintf("RING-CHANGE\t%s\tup\t%s", ip, tag), nil)
}

// LookupBackend returns the backend for the given username or nil if ring is empty.
func (s *Server) LookupBackend(username string) *ring.Backend {
	return s.ring.LookupBackend(username)
}

// RecordUser writes a user→backend mapping into the user directory.
func (s *Server) RecordUser(username, backendAddr string) {
	s.userDir.Set(username, backendAddr, false)
}

// backendTag looks up the tag for a backend IP, returning "" if not found.
func (s *Server) backendTag(ip string) string {
	for _, b := range s.ring.Backends() {
		if b.IP == ip {
			return b.Tag
		}
	}
	return ""
}
