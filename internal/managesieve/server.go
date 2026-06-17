package managesieve

import (
	"bufio"
	"context"
	"crypto/tls"
	"log/slog"
	"net"

	"github.com/0kaba0hub/yarilo/internal/loginproto"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/locks"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// Server is the yarilo ManageSieve backend.
type Server struct {
	opts Options
}

// Options configures the ManageSieve backend server.
type Options struct {
	// Locker coordinates cross-process writes to script files.
	Locker locks.Locker
	// Resolver maps usernames to home directories.
	Resolver *mailbox.Resolver
	// Config holds protocol-level tunables (max script size).
	Config config.ManageSieveProtocolConfig
	// AuthAddr is the host:port of yarilo-auth used by the PreambleListener
	// to verify session tokens forwarded by the login pod.
	AuthAddr string
	// AuthTLS is the mTLS client config for the yarilo-auth connection.
	AuthTLS *tls.Config
}

// New creates a ManageSieve server with the given options.
func New(opts Options) *Server {
	return &Server{opts: opts}
}

// ServeManageSieve wraps ln with a PreambleListener (when AuthAddr is set),
// accepts pre-authenticated connections from the ManageSieve login pod,
// and handles RFC 5804 sessions until ctx is cancelled.
func (srv *Server) ServeManageSieve(ctx context.Context, ln net.Listener) error {
	if srv.opts.AuthAddr != "" {
		ln = &loginproto.PreambleListener{
			Listener:        ln,
			AuthAddr:        srv.opts.AuthAddr,
			AuthTLS:         srv.opts.AuthTLS,
			ExpectedService: "managesieve",
		}
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				slog.Error("managesieve: accept", "err", err)
				return err
			}
		}
		go srv.handleConn(ctx, conn)
	}
}

func (srv *Server) handleConn(ctx context.Context, conn net.Conn) {
	pc, ok := conn.(*loginproto.PreambleConn)
	if !ok {
		slog.Error("managesieve: connection without preamble", "addr", conn.RemoteAddr())
		conn.Close()
		return
	}

	username := pc.Username
	userInfo := srv.opts.Resolver.UserInfo(username, "")

	slog.Info("managesieve: session started", "user", username, "session", pc.SessionID)

	maxSize := srv.opts.Config.MaxScriptSize
	if maxSize <= 0 {
		maxSize = 64 * 1024
	}

	sess := &session{
		conn:     conn,
		r:        bufio.NewReader(conn),
		w:        bufio.NewWriter(conn),
		username: username,
		homeDir:  userInfo.Home,
		locker:   srv.opts.Locker,
		maxSize:  maxSize,
	}
	sess.serve(ctx)
	slog.Info("managesieve: session ended", "user", username)
}
