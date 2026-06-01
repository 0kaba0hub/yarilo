// Package backend wires all server components for the backend (or single) mode.
package backend

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	_ "github.com/0kaba0hub/yarilo/pkg/dict/drivers/all" // register all dict drivers

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	authsql "github.com/0kaba0hub/yarilo/internal/auth/sql"
	"github.com/0kaba0hub/yarilo/internal/connlimit"
	imapsvr "github.com/0kaba0hub/yarilo/internal/imap"
	"github.com/0kaba0hub/yarilo/internal/lmtp"
	pop3svr "github.com/0kaba0hub/yarilo/internal/pop3"
	"github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/mdbox"
	submsvr "github.com/0kaba0hub/yarilo/internal/submission"
	submproxy "github.com/0kaba0hub/yarilo/internal/submission/proxy"
	"github.com/0kaba0hub/yarilo/internal/telemetry"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/dict"
	"github.com/0kaba0hub/yarilo/pkg/locks"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
	"github.com/0kaba0hub/yarilo/pkg/mtls"
)

// Server is the yarilo backend (or single-node) server.
type Server struct {
	cfg        *config.Config
	telem      *telemetry.Server
	imap       *imapsvr.Server // nil if neither IMAP nor IMAPS is active
	pop3       *pop3svr.Server // nil if neither POP3 nor POP3S is active
	submission *submsvr.Server // nil if no Submission/Submissions is active
	lmtp       *lmtp.Server    // nil if LMTP not configured
	locker     locks.Locker    // cross-process write coordinator; nil = disabled
}

// Close releases backend resources. Currently this means closing the
// yarilo-locks client (if any). Idempotent. Session binaries should defer
// Close after backend.New for clean lock release; without this the locker
// connection drops on FD reap and the server reclaims locks via TTL (~30s).
func (s *Server) Close() error {
	if s.locker != nil {
		return s.locker.Close()
	}
	return nil
}

// New creates and wires all components according to cfg.
func New(cfg *config.Config) (*Server, error) {
	// ---- auth ----
	passdbs, err := buildPassdbs(cfg.Auth.Passdb)
	if err != nil {
		return nil, fmt.Errorf("backend: auth: %w", err)
	}
	authChain := protocol.Chain(passdbs)

	// ---- storage ----
	if cfg.Storage.MaildirRoot == "" {
		cfg.Storage.MaildirRoot = "/var/mail/vhosts"
	}
	if cfg.Storage.MailHomeTemplate == "" {
		cfg.Storage.MailHomeTemplate = "%d/%n"
	}
	resolver := &mailbox.Resolver{
		Root:         cfg.Storage.MaildirRoot,
		HomeTemplate: cfg.Storage.MailHomeTemplate,
	}
	locker, err := buildLocksClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("backend: locks_client: %w", err)
	}
	mbox := buildMailbox(cfg.Storage, locker)
	idx := file.New(file.WithLocker(locker))

	// ---- per-namespace mailbox driver overrides ----
	// Builds a separate MailboxBackend instance per distinct
	// non-default driver referenced from cfg.Namespaces[*].Location.
	// Namespaces using the global driver are absent from the map and
	// resolve at session-open time to the global mbox.
	nsMailboxes, err := buildNamespaceMailboxes(cfg.Namespaces, cfg.Storage.Mailbox, locker)
	if err != nil {
		return nil, fmt.Errorf("backend: namespace mailboxes: %w", err)
	}

	// ---- dicts ----
	metadataDict, err := buildDict(cfg.Dicts, "metadata")
	if err != nil {
		return nil, fmt.Errorf("backend: dicts.metadata: %w", err)
	}

	// ---- shared connection limiter (IMAP + POP3) ----
	connLimiter := connlimit.New(cfg.General.Limits.MaxUserIPConnections)

	// ---- HAProxy / XClient shared nets ----
	haproxyNets := parseCIDRs(cfg.General.HAProxy.TrustedNets)
	xclientNets := parseCIDRs(cfg.General.XClient.TrustedNets)
	haproxyTimeout := time.Duration(cfg.General.HAProxy.Timeout) * time.Second

	// ---- IMAP ----
	var imapServer *imapsvr.Server
	svcs := cfg.Services
	if svcs.IMAP.Active() || svcs.IMAPS.Active() {
		primary := firstActive(svcs.IMAPS, svcs.IMAP)
		var imapTLS *tls.Config
		if svcs.IMAPS.Active() {
			t, err := buildTLS(cfg, svcs.IMAPS, alpnIMAP)
			if err != nil {
				return nil, fmt.Errorf("backend: IMAPS TLS: %w", err)
			}
			imapTLS = t
		}
		p := cfg.Protocol.IMAP
		imapServer = imapsvr.New(imapsvr.Options{
			Addr:               listenAddr(svcs.IMAPS),
			AddrPlain:          listenAddr(svcs.IMAP),
			TLSConfig:          imapTLS,
			Mailbox:            mbox,
			Index:              idx,
			Resolver:           resolver,
			Auth:               authChain,
			ProxyProtocol:      primary.HAProxy,
			HAProxyTimeout:     haproxyTimeout,
			HAProxyTrustedNets: haproxyNets,
			XClient:            primary.XClient,
			XClientTrustedNets: xclientNets,
			DisablePlainAuth:   primary.DisablePlainAuth,
			IdleNotifyInterval: time.Duration(p.IdleNotifyInterval) * time.Second,
			MaxLineLength:      p.MaxLineLength,
			ConnLimit:          connLimiter,
			IDSend:             p.IDSend,
			LoginGreeting:      p.LoginGreeting,
			LogoutFormat:       p.LogoutFormat,
			ClientWorkarounds:  imapsvr.ParseIMAPWorkarounds(p.ClientWorkarounds),
			Locker:             locker,
			SpecialUseDefaults: p.SpecialUseDefaults,
			MetadataDict:       metadataDict,
			ACLEnabled:         p.ACL.Enabled,
			Namespaces:         buildNamespaces(cfg.Namespaces),
			NamespaceMailboxes: nsMailboxes,
		})
	}

	// ---- POP3 ----
	var pop3Server *pop3svr.Server
	if svcs.POP3.Active() || svcs.POP3S.Active() {
		primary := firstActive(svcs.POP3S, svcs.POP3)
		var pop3TLS *tls.Config
		if svcs.POP3S.Active() {
			t, err := buildTLS(cfg, svcs.POP3S, alpnPOP3)
			if err != nil {
				return nil, fmt.Errorf("backend: POP3S TLS: %w", err)
			}
			pop3TLS = t
		}
		p := cfg.Protocol.POP3
		pop3Server = pop3svr.New(pop3svr.Options{
			Addr:               listenAddr(svcs.POP3S),
			AddrPlain:          listenAddr(svcs.POP3),
			TLSConfig:          pop3TLS,
			Mailbox:            mbox,
			Index:              idx,
			Resolver:           resolver,
			Auth:               authChain,
			ProxyProtocol:      primary.HAProxy,
			HAProxyTimeout:     haproxyTimeout,
			HAProxyTrustedNets: haproxyNets,
			XClient:            primary.XClient,
			XClientTrustedNets: xclientNets,
			DisablePlainAuth:   primary.DisablePlainAuth,
			NoFlagUpdates:      p.NoFlagUpdates,
			ReuseXUIDL:         p.ReuseXUIDL,
			UIDLFormat:         p.UIDLFormat,
			UIDLDuplicates:     p.UIDLDuplicates,
			EnableLast:         p.EnableLast,
			DeleteType:         p.DeleteType,
			DeletedFlag:        p.DeletedFlag,
			SaveUIDL:           p.SaveUIDL,
			LockSession:        p.LockSession,
			ConnLimit:          connLimiter,
			Locker:             locker,
		})
	}

	// ---- SMTP submission ----
	var smtpServer *submsvr.Server
	if svcs.Submission.Active() || svcs.Submissions.Active() {
		primary := firstActive(svcs.Submission, svcs.Submissions)

		var submissionProxy *submproxy.Submission
		if cfg.Protocol.Submission.Relay.Host != "" {
			submissionProxy = submproxy.New(cfg.Protocol.Submission.Relay, cfg.Protocol.Submission.Hostname)
		}

		var submissionTLS *tls.Config
		if primary.SSLMode != "no" && primary.SSLMode != "" {
			t, err := buildTLS(cfg, primary, alpnSMTP)
			if err != nil {
				return nil, fmt.Errorf("backend: submission TLS: %w", err)
			}
			submissionTLS = t
		}

		smtpServer = submsvr.New(submsvr.Options{
			HAProxy:          primary.HAProxy,
			HAProxyTimeout:   haproxyTimeout,
			HAProxyNets:      haproxyNets,
			XClient:          primary.XClient,
			XClientNets:      xclientNets,
			DisablePlainAuth: primary.DisablePlainAuth,
			TLSConfig:        submissionTLS,
			Config:           cfg.Protocol.Submission,
			Auth:             chainAuth{authChain},
			Proxy:            submissionProxy,
		})
	}

	// ---- LMTP ----
	var lmtpServer *lmtp.Server
	if svcs.LMTP.Active() {
		var lmtpTLS *tls.Config
		if svcs.LMTP.SSLMode == "starttls" {
			t, err := buildTLS(cfg, svcs.LMTP)
			if err != nil {
				return nil, fmt.Errorf("backend: LMTP STARTTLS: %w", err)
			}
			lmtpTLS = t
		}
		lmtpServer = lmtp.New(lmtp.Options{
			Hostname:           cfg.Protocol.Submission.Hostname,
			Config:             cfg.Protocol.LMTP,
			Mailbox:            mbox,
			Index:              idx,
			Resolver:           resolver,
			ProxyProtocol:      svcs.LMTP.HAProxy,
			HAProxyTimeout:     haproxyTimeout,
			HAProxyTrustedNets: haproxyNets,
			XClient:            svcs.LMTP.XClient,
			XClientTrustedNets: xclientNets,
			TLSConfig:          lmtpTLS,
			Locker:             locker,
			AnvilAddr:          cfg.AnvilService.Listen,
		})
	}

	// ---- telemetry ----
	telemAddr := cfg.Telemetry.Listen
	if telemAddr == "" {
		telemAddr = ":8080"
	}
	telem := telemetry.New(telemAddr)

	return &Server{
		cfg:        cfg,
		telem:      telem,
		imap:       imapServer,
		pop3:       pop3Server,
		submission: smtpServer,
		lmtp:       lmtpServer,
		locker:     locker,
	}, nil
}

// RunIMAP starts the IMAP/IMAPS listeners and telemetry, then blocks until ctx is cancelled.
func (s *Server) RunIMAP(ctx context.Context) error {
	go func() {
		if err := s.telem.ListenAndServe(ctx); err != nil {
			slog.Error("telemetry: server error", "err", err)
		}
	}()
	s.telem.SetReady(true)

	svcs := s.cfg.Services
	if s.imap == nil {
		slog.Warn("imap: no listeners configured")
		<-ctx.Done()
		return nil
	}
	if svcs.IMAPS.Active() {
		go func() {
			if err := s.imap.ListenAndServeTLS(); err != nil {
				slog.Error("imap: TLS server error", "err", err)
				os.Exit(1)
			}
		}()
	}
	if svcs.IMAP.Active() {
		go func() {
			if err := s.imap.ListenAndServe(); err != nil {
				slog.Error("imap: plain server error", "err", err)
				os.Exit(1)
			}
		}()
	}
	<-ctx.Done()
	return nil
}

// RunPOP3 starts the POP3/POP3S listeners and telemetry, then blocks until ctx is cancelled.
func (s *Server) RunPOP3(ctx context.Context) error {
	go func() {
		if err := s.telem.ListenAndServe(ctx); err != nil {
			slog.Error("telemetry: server error", "err", err)
		}
	}()
	s.telem.SetReady(true)

	svcs := s.cfg.Services
	if s.pop3 == nil {
		slog.Warn("pop3: no listeners configured")
		<-ctx.Done()
		return nil
	}
	if svcs.POP3S.Active() {
		go func() {
			if err := s.pop3.ListenAndServeTLS(); err != nil {
				slog.Error("pop3: TLS server error", "err", err)
				os.Exit(1)
			}
		}()
	}
	if svcs.POP3.Active() {
		go func() {
			if err := s.pop3.ListenAndServe(); err != nil {
				slog.Error("pop3: plain server error", "err", err)
				os.Exit(1)
			}
		}()
	}
	<-ctx.Done()
	return nil
}

// RunLMTP starts the LMTP listener and telemetry, then blocks until ctx is cancelled.
func (s *Server) RunLMTP(ctx context.Context) error {
	go func() {
		if err := s.telem.ListenAndServe(ctx); err != nil {
			slog.Error("telemetry: server error", "err", err)
		}
	}()
	s.telem.SetReady(true)

	svcs := s.cfg.Services
	if s.lmtp == nil || !svcs.LMTP.Active() {
		slog.Warn("lmtp: no listener configured")
		<-ctx.Done()
		return nil
	}
	go func() {
		ln, err := net.Listen("tcp", listenAddr(svcs.LMTP))
		if err != nil {
			slog.Error("lmtp: listen error", "err", err)
			os.Exit(1)
		}
		if svcs.LMTP.SSLMode == "ssl" {
			tlsCfg, err := buildTLS(s.cfg, svcs.LMTP)
			if err != nil {
				slog.Error("lmtp: TLS error", "err", err)
				os.Exit(1)
			}
			if tlsCfg != nil {
				ln = tls.NewListener(ln, tlsCfg)
			}
		}
		if err := s.lmtp.Serve(ln); err != nil {
			slog.Error("lmtp: server error", "err", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	return nil
}

// Run starts all configured servers and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	go func() {
		if err := s.telem.ListenAndServe(ctx); err != nil {
			slog.Error("telemetry: server error", "err", err)
		}
	}()
	s.telem.SetReady(true)

	svcs := s.cfg.Services

	// IMAP
	if s.imap != nil {
		if svcs.IMAPS.Active() {
			go func() {
				if err := s.imap.ListenAndServeTLS(); err != nil {
					slog.Error("imap: TLS server error", "err", err)
					os.Exit(1)
				}
			}()
		}
		if svcs.IMAP.Active() {
			go func() {
				if err := s.imap.ListenAndServe(); err != nil {
					slog.Error("imap: plain server error", "err", err)
					os.Exit(1)
				}
			}()
		}
	}

	// POP3
	if s.pop3 != nil {
		if svcs.POP3S.Active() {
			go func() {
				if err := s.pop3.ListenAndServeTLS(); err != nil {
					slog.Error("pop3: TLS server error", "err", err)
					os.Exit(1)
				}
			}()
		}
		if svcs.POP3.Active() {
			go func() {
				if err := s.pop3.ListenAndServe(); err != nil {
					slog.Error("pop3: plain server error", "err", err)
					os.Exit(1)
				}
			}()
		}
	}

	// Submission (STARTTLS)
	if s.submission != nil && svcs.Submission.Active() {
		go func() {
			ln, err := net.Listen("tcp", listenAddr(svcs.Submission))
			if err != nil {
				slog.Error("smtp: submission listen error", "err", err)
				os.Exit(1)
			}
			var tlsCfg *tls.Config
			if svcs.Submission.SSLMode == "ssl" {
				t, err := buildTLS(s.cfg, svcs.Submission, alpnSMTP)
				if err != nil {
					slog.Error("smtp: submission TLS error", "err", err)
					os.Exit(1)
				}
				tlsCfg = t
			}
			if err := s.submission.Serve(ln, tlsCfg); err != nil {
				slog.Error("smtp: submission server error", "err", err)
				os.Exit(1)
			}
		}()
	}

	// LMTP (port 24) — local delivery for external MTAs
	if s.lmtp != nil && svcs.LMTP.Active() {
		go func() {
			ln, err := net.Listen("tcp", listenAddr(svcs.LMTP))
			if err != nil {
				slog.Error("lmtp: listen error", "err", err)
				os.Exit(1)
			}
			if svcs.LMTP.SSLMode == "ssl" {
				tlsCfg, err := buildTLS(s.cfg, svcs.LMTP)
				if err != nil {
					slog.Error("lmtp: TLS error", "err", err)
					os.Exit(1)
				}
				if tlsCfg != nil {
					ln = tls.NewListener(ln, tlsCfg)
				}
			}
			if err := s.lmtp.Serve(ln); err != nil {
				slog.Error("lmtp: server error", "err", err)
				os.Exit(1)
			}
		}()
	}

	// Submissions (SSL-only, port 465)
	if s.submission != nil && svcs.Submissions.Active() {
		go func() {
			ln, err := net.Listen("tcp", listenAddr(svcs.Submissions))
			if err != nil {
				slog.Error("smtp: submissions listen error", "err", err)
				os.Exit(1)
			}
			tlsCfg, err := buildTLS(s.cfg, svcs.Submissions, alpnSMTP)
			if err != nil {
				slog.Error("smtp: submissions TLS error", "err", err)
				os.Exit(1)
			}
			if err := s.submission.Serve(ln, tlsCfg); err != nil {
				slog.Error("smtp: submissions server error", "err", err)
				os.Exit(1)
			}
		}()
	}

	<-ctx.Done()
	return nil
}

// ---- helpers ----------------------------------------------------------------

// firstActive returns the first non-nil active ServiceConfig from the list.
func firstActive(svcs ...*config.ServiceConfig) *config.ServiceConfig {
	for _, s := range svcs {
		if s.Active() {
			return s
		}
	}
	return nil
}

// listenAddr converts a ServiceConfig into a TCP listen address.
func listenAddr(svc *config.ServiceConfig) string {
	if svc == nil {
		return ""
	}
	return fmt.Sprintf(":%d", svc.Port)
}

// buildTLS resolves TLS config for a service (merges general.ssl + per-service override)
// and sets ALPN protocols matching the protocol (IANA RFC 7301 names).
// Clients sending ALPN must match one of the listed protocols; clients without
// ALPN are accepted (Dovecot 2.4 semantics).
func buildTLS(cfg *config.Config, svc *config.ServiceConfig, alpn ...string) (*tls.Config, error) {
	ssl := cfg.ResolveSSL(svc)
	if ssl.TLSCert == "" {
		return nil, nil
	}
	tlsCfg, err := config.BuildTLSConfig(ssl)
	if err != nil {
		return nil, err
	}
	if len(alpn) > 0 {
		tlsCfg.NextProtos = alpn
	}
	return tlsCfg, nil
}

// ALPN protocol identifiers (IANA RFC 7301 registry).
const (
	alpnIMAP = "imap"
	alpnPOP3 = "pop3"
	alpnSMTP = "smtp"
)

func parseCIDRs(cidrs []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, s := range cidrs {
		_, ipnet, err := net.ParseCIDR(s)
		if err != nil {
			slog.Warn("backend: invalid CIDR, skipping", "cidr", s, "err", err)
			continue
		}
		nets = append(nets, ipnet)
	}
	return nets
}

// chainAuth adapts protocol.Chain to smtp.Authenticator.
type chainAuth struct{ c protocol.Chain }

func (a chainAuth) AuthPlain(username, password string) error {
	resp, err := a.c.Authenticate(username, password, "smtp")
	if err != nil {
		return fmt.Errorf("smtp/auth: %w", err)
	}
	if resp == nil || resp.Result != protocol.AuthOK {
		return fmt.Errorf("smtp/auth: authentication failed")
	}
	return nil
}

func buildMailbox(cfg config.StorageConfig, locker locks.Locker) mailbox.MailboxBackend {
	return buildMailboxByDriver(cfg.Mailbox, locker)
}

// buildMailboxByDriver constructs a MailboxBackend for the named
// driver. Defaults to maildir for unknown / empty drivers so an
// operator's typo does not crash startup. Reused by buildMailbox
// (global default from cfg.Storage.Mailbox) and by
// buildNamespaceMailboxes (per-namespace override from
// cfg.Namespaces[*].Location).
func buildMailboxByDriver(driver string, locker locks.Locker) mailbox.MailboxBackend {
	switch strings.ToLower(driver) {
	case "sdbox", "dbox":
		return dboxv2.New(dboxv2.WithLocker(locker))
	case "mdbox":
		return mdbox.New(mdbox.WithLocker(locker))
	default:
		return maildir.New(maildir.WithLocker(locker))
	}
}

// buildNamespaceMailboxes constructs the per-namespace MailboxBackend
// override map for cfg.Namespaces. A namespace declared with a
// `location:` URL whose driver prefix differs from cfg.Storage.Mailbox
// gets its own backend instance; namespaces without a location: or
// using the same driver as the global default are absent from the map
// and resolve at session-open time to the global backend.
//
// The override map is keyed by namespace prefix (same key the IMAP
// session dispatcher uses). Same-driver namespaces share their
// Backend instance to keep the in-memory footprint small.
func buildNamespaceMailboxes(namespaces []config.NamespaceConfig, globalDriver string, locker locks.Locker) (map[string]mailbox.MailboxBackend, error) {
	if len(namespaces) == 0 {
		return nil, nil
	}
	globalDriver = strings.ToLower(globalDriver)
	if globalDriver == "" {
		globalDriver = "maildir"
	}
	byDriver := make(map[string]mailbox.MailboxBackend) // shared per driver string
	overrides := map[string]mailbox.MailboxBackend{}
	for _, ns := range namespaces {
		if ns.Location == "" {
			// Personal-style namespace inheriting the global default.
			continue
		}
		loc, ok, err := mailbox.ParseLocation(ns.Location, nil)
		if err != nil {
			return nil, fmt.Errorf("namespace %q: %w", ns.Prefix, err)
		}
		if !ok {
			continue
		}
		drv := strings.ToLower(loc.Driver)
		if drv == globalDriver {
			// Same driver as global default — no override needed; the
			// session opens against the global Mailbox backend.
			continue
		}
		b, exists := byDriver[drv]
		if !exists {
			b = buildMailboxByDriver(drv, locker)
			byDriver[drv] = b
			slog.Info("backend: per-namespace mailbox backend built", "driver", drv, "ns", ns.Prefix)
		}
		overrides[ns.Prefix] = b
	}
	if len(overrides) == 0 {
		return nil, nil
	}
	return overrides, nil
}

// buildNamespaces translates cfg.Namespaces into the wire-protocol
// shape the IMAP server consumes. An empty slice in => empty slice
// out; the server applies its built-in single-personal-namespace
// fallback so pre-v1.20 deployments without a namespaces: block keep
// working unchanged.
//
// Separator defaults to "/" when omitted; non-single-rune values are
// dropped to "/" with a warning so a misconfigured yaml does not
// produce a malformed NAMESPACE response.
func buildNamespaces(cfg []config.NamespaceConfig) []imapsvr.NamespaceSpec {
	if len(cfg) == 0 {
		return nil
	}
	out := make([]imapsvr.NamespaceSpec, 0, len(cfg))
	for i, ns := range cfg {
		t := strings.ToLower(strings.TrimSpace(ns.Type))
		var nsType imapsvr.NamespaceType
		switch t {
		case "personal":
			nsType = imapsvr.NamespacePersonal
		case "other", "other_users":
			nsType = imapsvr.NamespaceOther
		case "shared":
			nsType = imapsvr.NamespaceShared
		default:
			slog.Warn("backend: skipping namespace with unknown type",
				"index", i, "type", ns.Type, "prefix", ns.Prefix)
			continue
		}
		sep := '/'
		if rs := []rune(ns.Separator); len(rs) == 1 {
			sep = rs[0]
		} else if ns.Separator != "" {
			slog.Warn("backend: namespace separator must be a single character, defaulting to /",
				"index", i, "separator", ns.Separator)
		}
		out = append(out, imapsvr.NamespaceSpec{
			Type:      nsType,
			Prefix:    ns.Prefix,
			Separator: sep,
			List:      ns.List,
			Location:  ns.Location,
		})
	}
	return out
}

// buildDict opens the named dict from cfg.Dicts, returning nil when the
// entry is absent. The caller decides whether nil is acceptable —
// IMAP METADATA tolerates a nil dict (the feature degrades to "Metadata
// storage not configured"); other consumers may require a non-nil
// result and error out at startup.
func buildDict(dicts map[string]config.DictConfig, name string) (dict.Dict, error) {
	cfg, ok := dicts[name]
	if !ok {
		return nil, nil
	}
	if cfg.Driver == "" {
		return nil, fmt.Errorf("dict %q has empty driver", name)
	}
	d, err := dict.Open(dict.Config{Driver: cfg.Driver, Settings: cfg.Settings})
	if err != nil {
		return nil, fmt.Errorf("open dict %q: %w", name, err)
	}
	slog.Info("backend: dict opened", "name", name, "driver", cfg.Driver)
	return d, nil
}

// buildLocksClient constructs a yarilo-locks client per cfg.LocksClient.
// Returns (nil, nil) when locks are disabled (Mode == ""). The returned
// Locker must be closed by Server.Close.
func buildLocksClient(cfg *config.Config) (locks.Locker, error) {
	lc := cfg.LocksClient
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	switch lc.Mode {
	case "":
		return nil, nil
	case "embedded":
		if lc.Socket == "" {
			return nil, fmt.Errorf("locks_client.socket is required for embedded mode")
		}
		return locks.NewClient(ctx, locks.DialUnix(lc.Socket))
	case "remote":
		if len(lc.Endpoints) == 0 {
			return nil, fmt.Errorf("locks_client.endpoints must list at least one host:port for remote mode")
		}
		var tlsCfg *tls.Config
		if cfg.InternalTLS.Enabled {
			t, err := mtls.ClientConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA)
			if err != nil {
				return nil, fmt.Errorf("locks_client mtls: %w", err)
			}
			tlsCfg = t
		}
		// Single-endpoint connect for now; failover across Endpoints is a
		// follow-up (custom Dialer iterating the list until first success).
		return locks.NewClient(ctx, locks.DialTLS(lc.Endpoints[0], tlsCfg))
	default:
		return nil, fmt.Errorf("locks_client: unknown mode %q (want remote | embedded | \"\")", lc.Mode)
	}
}

func buildPassdbs(entries []config.PassdbEntry) ([]protocol.Passdb, error) {
	var dbs []protocol.Passdb
	for _, e := range entries {
		switch strings.ToLower(e.Driver) {
		case "sqlite", "mysql", "postgres":
			db, err := authsql.New(authsql.Config{
				Driver:            e.Driver,
				DSN:               e.DSN,
				PasswordQuery:     e.PasswordQuery,
				UserQuery:         e.UserQuery,
				IterateQuery:      e.IterateQuery,
				DefaultPassScheme: e.DefaultPassScheme,
				SkipSchema:        e.SkipSchema,
			})
			if err != nil {
				return nil, fmt.Errorf("passdb %s: %w", e.Driver, err)
			}
			dbs = append(dbs, db)
		default:
			return nil, fmt.Errorf("unknown passdb driver: %s", e.Driver)
		}
	}
	return dbs, nil
}
