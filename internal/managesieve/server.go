package managesieve

import (
	"bufio"
	"context"
	"crypto/tls"
	"log/slog"
	"net"

	"github.com/yarilomail/yarilo/internal/loginproto"
	"github.com/yarilomail/yarilo/internal/sieve"
	"github.com/yarilomail/yarilo/pkg/authclient"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Server is the yarilo ManageSieve backend.
type Server struct {
	opts Options
}

// Options configures the ManageSieve backend server.
type Options struct {
	// Locker coordinates cross-process writes to script files.
	Locker locks.Locker
	// DefaultName is the reserved Sieve script name (sieve.default_name). Default: "yarilo".
	DefaultName string
	// Resolver maps usernames to home directories.
	Resolver *mailbox.Resolver
	// Config holds protocol-level tunables.
	Config config.ManageSieveProtocolConfig
	// MaxScriptSize is the script-size limit, which lives in the sieve section
	// (sieve_max_script_size): the managesieve duplicate was folded onto it
	// (#1286), so the server is handed the resolved value rather than reading
	// one of two keys and choosing.
	MaxScriptSize int
	// SieveExtensions is the whitelist of permitted Sieve extensions.
	// Corresponds to sieve.sieve_extensions in yarilo.yaml. Empty = allow all.
	SieveExtensions []string
	// ScriptsDriver selects the script storage backend: "fs" (default) or "redis".
	ScriptsDriver string
	// ScriptsDict is the dict instance used when ScriptsDriver is "redis".
	ScriptsDict dict.Dict
	// AuthAddr is the host:port of yarilo-auth login protocol used by the
	// PreambleListener to verify session tokens forwarded by the login pod.
	AuthAddr string
	// AuthTLS is the mTLS client config for the yarilo-auth login connection.
	AuthTLS     *tls.Config
	PreambleTLS *tls.Config // internal mTLS on the data path (#824)
	// MasterAddr is the host:port of yarilo-auth master protocol for userdb lookups.
	MasterAddr string
	// MasterTLS is the mTLS client config for the yarilo-auth master connection.
	MasterTLS *tls.Config
	// MasterPool serves the session userdb lookup from a shared connection
	// instead of dialling one per session (#1419).
	MasterPool *authclient.Pool
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
			MasterAddr:      srv.opts.MasterAddr,
			MasterTLS:       srv.opts.MasterTLS,
			MasterPool:      srv.opts.MasterPool,
			ExpectedService: "managesieve",
			TLSConfig:       srv.opts.PreambleTLS,
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
	userInfo := srv.opts.Resolver.UserInfo(username, pc.Home)

	slog.Info("managesieve: session started", "user", username, "session", pc.SessionID)

	maxSize := srv.opts.MaxScriptSize
	if maxSize <= 0 {
		maxSize = 64 * 1024
	}

	defaultName := srv.opts.DefaultName
	if defaultName == "" {
		defaultName = sieve.FallbackDefaultName
	}
	sess := &session{
		conn:              conn,
		r:                 bufio.NewReader(conn),
		w:                 bufio.NewWriter(conn),
		username:          username,
		homeDir:           userInfo.Home,
		store:             sieve.NewScriptStore(srv.opts.ScriptsDriver, defaultName, srv.opts.Locker, srv.opts.ScriptsDict),
		maxSize:           maxSize,
		allowedExtensions: srv.opts.SieveExtensions,
		sid:               pc.SessionID,
	}
	sess.serve(ctx)
	slog.Info("managesieve: session ended", "sid", pc.SessionID, "user", username)
}
