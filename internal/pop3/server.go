// Package pop3 implements a POP3 server (RFC 1939).
// Supports POP3S (port 995), STARTTLS (port 110), HAProxy PROXY protocol,
// and XCLIENT from trusted relay infrastructure.
package pop3

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	proxyproto "github.com/pires/go-proxyproto"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	"github.com/0kaba0hub/yarilo/internal/connlimit"
	"github.com/0kaba0hub/yarilo/pkg/locks"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// Options configures the POP3 server.
type Options struct {
	Addr               string // POP3S address, e.g. ":995"
	AddrPlain          string // STARTTLS address, e.g. ":110"
	TLSConfig          *tls.Config
	Mailbox            mailbox.MailboxBackend
	Index              mailbox.IndexBackend
	Resolver           *mailbox.Resolver
	Auth               protocol.Authenticator
	ProxyProtocol      bool
	HAProxyTimeout     time.Duration
	HAProxyTrustedNets []*net.IPNet
	XClient            bool
	XClientTrustedNets []*net.IPNet
	DisablePlainAuth   bool // reject USER/PASS without TLS
	// POP3-specific behaviour (Dovecot parity)
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

	// Locker is the cross-process write coordinator. When non-nil, the
	// QUIT-time hard-delete batch runs under a single X lock on
	// mbox:<user>:INBOX so concurrent IMAP/LMTP writers observe an
	// atomic before-or-after view across the whole expunge. Nil falls
	// back to per-message locking via the storage backends (correct but
	// not batch-atomic).
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
	ln = s.wrapProxy(ln)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.newSession(conn).serve()
	}
}

func (s *Server) wrapProxy(ln net.Listener) net.Listener {
	if !s.opts.ProxyProtocol {
		return ln
	}
	timeout := s.opts.HAProxyTimeout
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	return &proxyproto.Listener{
		Listener:          ln,
		ReadHeaderTimeout: timeout,
		Policy:            proxyPolicy(s.opts.HAProxyTrustedNets),
	}
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
