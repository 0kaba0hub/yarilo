// Package protocol implements the yarilo-auth TCP+mTLS protocol.
// Server → Client handshake, then AUTH/CONT/CANCEL commands.
package protocol

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	protoName = "yarilo-auth"
	majorVer  = 1
	minorVer  = 0
	maxLine   = 16384
)

// AuthResult is the outcome of a single authentication attempt.
type AuthResult int

const (
	AuthOK AuthResult = iota
	AuthFail
	AuthTempFail
)

// AuthResponse is the final response sent back to the client session.
type AuthResponse struct {
	Result   AuthResult
	Username string
	Home     string
	MailLoc  string
	Proxy    bool
	Host     string
	Port     int
}

// Passdb is the interface that passdb backends implement.
type Passdb interface {
	// Authenticate verifies credentials and returns user fields.
	// Returns nil response (no error) to pass to the next passdb in chain.
	Authenticate(username, password, service string) (*AuthResponse, error)
}

// Chain implements Passdb by trying each entry in order.
// First non-nil response wins; nil + nil means "unknown user, try next".
type Chain []Passdb

func (c Chain) Authenticate(username, password, service string) (*AuthResponse, error) {
	for _, db := range c {
		res, err := db.Authenticate(username, password, service)
		if err != nil {
			return &AuthResponse{Result: AuthTempFail}, err
		}
		if res != nil {
			return res, nil
		}
	}
	return &AuthResponse{Result: AuthFail}, nil
}

// Server is the yarilo-auth UNIX-socket server.
type Server struct {
	passdbs []Passdb
	connUID atomic.Uint64
	pid     int
	cookie  string
}

// NewServer creates a new auth server with the given passdb chain.
func NewServer(passdbs []Passdb) *Server {
	cookie := make([]byte, 16)
	rand.Read(cookie) //nolint:errcheck
	return &Server{
		passdbs: passdbs,
		pid:     os.Getpid(),
		cookie:  hex.EncodeToString(cookie),
	}
}

// ListenAndServe starts the auth TCP server. When tlsCfg is non-nil the
// listener uses mTLS (TLS 1.3, RequireAndVerifyClientCert). Blocks until ctx
// is cancelled; active sessions drain before the function returns.
func (s *Server) ListenAndServe(ctx context.Context, addr string, tlsCfg *tls.Config) error {
	var ln net.Listener
	var err error
	if tlsCfg != nil {
		ln, err = tls.Listen("tcp", addr, tlsCfg)
	} else {
		ln, err = net.Listen("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("auth: listen %s: %w", addr, err)
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
			return fmt.Errorf("auth: accept: %w", err)
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
	cuid := s.connUID.Add(1)
	rd := bufio.NewReaderSize(conn, maxLine)

	// Server → Client handshake
	fmt.Fprintf(conn, "VERSION\t%s\t%d\t%d\n", protoName, majorVer, minorVer)
	fmt.Fprintf(conn, "MECH\tPLAIN\t\n")
	fmt.Fprintf(conn, "MECH\tLOGIN\t\n")
	fmt.Fprintf(conn, "SPID\t%d\n", s.pid)
	fmt.Fprintf(conn, "CUID\t%d\n", cuid)
	fmt.Fprintf(conn, "COOKIE\t%s\n", s.cookie)
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
		case "CPID":
			// client pid — ignore for now
		case "AUTH":
			s.handleAuth(conn, fields)
		case "CONT":
			// SASL continuation — not needed for PLAIN
		case "CANCEL":
			// cancel pending auth
		}
	}
}

func (s *Server) handleAuth(conn net.Conn, fields []string) {
	if len(fields) < 3 {
		return
	}
	id := fields[1]
	mech := fields[2]

	var service, resp string
	for _, f := range fields[3:] {
		if strings.HasPrefix(f, "service=") {
			service = strings.TrimPrefix(f, "service=")
		}
		if strings.HasPrefix(f, "resp=") {
			resp = strings.TrimPrefix(f, "resp=")
		}
	}
	_ = service

	username, password, ok := parsePlain(mech, resp)
	if !ok {
		fmt.Fprintf(conn, "FAIL\t%s\treason=bad-credentials\n", id)
		return
	}

	res, err := s.authenticate(username, password, service)
	if err != nil || res == nil || res.Result != AuthOK {
		if err != nil || (res != nil && res.Result == AuthTempFail) {
			fmt.Fprintf(conn, "FAIL\t%s\ttemp_fail\n", id)
		} else {
			fmt.Fprintf(conn, "FAIL\t%s\n", id)
		}
		return
	}

	reply := fmt.Sprintf("OK\t%s\tuser=%s", id, res.Username)
	if res.Home != "" {
		reply += "\thome=" + res.Home
	}
	if res.MailLoc != "" {
		reply += "\tmail=" + res.MailLoc
	}
	fmt.Fprintln(conn, reply)
}

func (s *Server) authenticate(username, password, service string) (*AuthResponse, error) {
	for _, db := range s.passdbs {
		res, err := db.Authenticate(username, password, service)
		if err != nil {
			return &AuthResponse{Result: AuthTempFail}, err
		}
		if res != nil {
			return res, nil
		}
	}
	return &AuthResponse{Result: AuthFail}, nil
}

// parsePlain decodes PLAIN SASL or treats resp as "user\0pass" (LOGIN).
func parsePlain(mech, resp string) (username, password string, ok bool) {
	// PLAIN: authzid\0authid\0passwd (base64 already decoded by client field)
	if mech == "PLAIN" || mech == "LOGIN" {
		parts := strings.SplitN(resp, "\x00", 3)
		if len(parts) == 3 {
			return parts[1], parts[2], true
		}
		if len(parts) == 2 {
			return parts[0], parts[1], true
		}
	}
	return "", "", false
}
