// Package pop3 implements a POP3 server (RFC 1939).
// Supports POP3S (port 995), STARTTLS (port 110), HAProxy PROXY protocol,
// and yarilo login-pod preamble for pre-authenticated sessions.
package pop3

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	proxyproto "github.com/pires/go-proxyproto"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
	"github.com/yarilomail/yarilo/internal/connlimit"
	"github.com/yarilomail/yarilo/internal/loginproto"
	"github.com/yarilomail/yarilo/pkg/authclient"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Options configures the POP3 server.
type Options struct {
	Addr      string // POP3S address, e.g. ":995"
	AddrPlain string // STARTTLS address, e.g. ":110"
	TLSConfig *tls.Config
	Mailbox   mailbox.MailboxBackend
	// MailboxByDriver (optional) returns a MailboxBackend for a driver name
	// ("maildir", "sdbox", "mdbox"). Without it dbox users are read through
	// the global Mailbox backend and see 0 messages.
	MailboxByDriver    func(driver string) mailbox.MailboxBackend
	Index              mailbox.IndexBackend
	Resolver           *mailbox.Resolver
	Auth               protocol.Authenticator
	ProxyProtocol      bool
	HAProxyTimeout     time.Duration
	HAProxyTrustedNets []*net.IPNet
	// AuthAddr is the host:port of yarilo-auth login protocol used by the
	// PreambleListener to verify session tokens forwarded by login pods.
	AuthAddr    string
	AuthTLS     *tls.Config
	PreambleTLS *tls.Config // internal mTLS on the data path (#824)
	// MasterAddr is the host:port of yarilo-auth master protocol for userdb lookups.
	MasterAddr string
	MasterTLS  *tls.Config
	// MasterPool serves the session userdb lookup from a shared connection
	// instead of dialling one per session (#1419).
	MasterPool       *authclient.Pool
	DisablePlainAuth bool // reject USER/PASS without TLS
	// POP3-specific behaviour
	NoFlagUpdates  bool               // pop3_no_flag_updates: skip \Seen on RETR
	ReuseXUIDL     bool               // pop3_reuse_xuidl: use X-UIDL header (migration)
	UIDLFormat     string             // pop3_uidl_format: %u=UID %v=UIDValidity %f=filename %g=GUID
	UIDLDuplicates string             // pop3_uidl_duplicates: allow | rename
	EnableLast     bool               // pop3_enable_last: LAST command (RFC 1460)
	DeleteType     string             // pop3_delete_type: expunge | flag
	DeletedFlag    string             // pop3_deleted_flag: IMAP flag for soft-delete
	SaveUIDL       bool               // pop3_save_uidl: persist UIDLs to index for stability across rebuilds
	LockSession    bool               // pop3_lock_session: dotlock file to prevent IMAP+POP3 conflicts
	ConnLimit      *connlimit.Limiter // per-user@IP connection limit; nil = unlimited
	// FailureDelay delays auth-failure replies so wire timing carries no
	// info about whether the user exists. Zero disables.
	FailureDelay time.Duration

	// OAuth2Enabled enables the OAUTHBEARER/XOAUTH2 SASL mechanisms.
	// Set when at least one OAuth provider is configured.
	OAuth2Enabled bool

	// Locker is the cross-process write coordinator. Non-nil runs the
	// QUIT-time expunge batch under one X lock on mbox:<user>:INBOX;
	// nil falls back to per-message locking (correct but not batch-atomic).
	Locker locks.Locker
}

// Server is the yarilo POP3 server.
type Server struct {
	opts  Options
	mu    sync.Mutex
	locks map[string]struct{} // per-user session locks
}

// New creates a POP3 server.
func New(opts Options) *Server {
	// Memoise the per-driver backend once, so a per-session backend selection
	// shares one write semaphore instead of building a fresh one each login
	// (#1149).
	opts.MailboxByDriver = mailbox.MemoizeByDriver(opts.MailboxByDriver)
	return &Server{opts: opts, locks: make(map[string]struct{})}
}

// ListenAndServeTLS starts the POP3S (TLS) listener.
func (s *Server) ListenAndServeTLS() error {
	if s.opts.TLSConfig == nil {
		return fmt.Errorf("pop3: TLS config required for POP3S")
	}
	ln, err := tls.Listen("tcp", s.opts.Addr, s.opts.TLSConfig)
	if err != nil {
		return err
	}
	slog.Info("pop3: listening (TLS)", "addr", s.opts.Addr)
	return s.Serve(ln)
}

// ListenAndServe starts the plain STARTTLS listener.
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.opts.AddrPlain)
	if err != nil {
		return err
	}
	slog.Info("pop3: listening (STARTTLS)", "addr", s.opts.AddrPlain)
	return s.Serve(ln)
}

// Serve accepts connections on the given listener.
func (s *Server) Serve(ln net.Listener) error {
	ln = s.wrapListeners(ln)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.newSession(conn).serve()
	}
}

func (s *Server) wrapListeners(ln net.Listener) net.Listener {
	if s.opts.ProxyProtocol {
		timeout := s.opts.HAProxyTimeout
		if timeout == 0 {
			timeout = 3 * time.Second
		}
		ln = &proxyproto.Listener{
			Listener:          ln,
			ReadHeaderTimeout: timeout,
			Policy:            proxyPolicy(s.opts.HAProxyTrustedNets),
		}
	}
	if s.opts.AuthAddr != "" {
		ln = &loginproto.PreambleListener{
			Listener:        ln,
			AuthAddr:        s.opts.AuthAddr,
			AuthTLS:         s.opts.AuthTLS,
			MasterAddr:      s.opts.MasterAddr,
			MasterTLS:       s.opts.MasterTLS,
			MasterPool:      s.opts.MasterPool,
			ExpectedService: "pop3",
			TLSConfig:       s.opts.PreambleTLS,
		}
	}
	return ln
}

// tryLock acquires an exclusive per-user session lock.
// Returns false if a session for this user is already active.
func (s *Server) tryLock(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.locks[key]; ok {
		return false
	}
	s.locks[key] = struct{}{}
	return true
}

func (s *Server) unlock(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.locks, key)
}

func proxyPolicy(nets []*net.IPNet) func(net.Addr) (proxyproto.Policy, error) {
	return func(upstream net.Addr) (proxyproto.Policy, error) {
		if len(nets) == 0 {
			return proxyproto.IGNORE, nil
		}
		tcp, ok := upstream.(*net.TCPAddr)
		if !ok {
			return proxyproto.IGNORE, nil
		}
		for _, n := range nets {
			if n.Contains(tcp.IP) {
				return proxyproto.USE, nil
			}
		}
		return proxyproto.IGNORE, nil
	}
}
