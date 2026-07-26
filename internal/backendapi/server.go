// Package backendapi is the backend-plane admin HTTP API.
//
// It exposes operator endpoints (dict / acl / quota / folder / user /
// mailbox / ...) against the local backend's storage and pkg/dict
// instances. One instance runs per backend tag (or one per
// standalone deployment).
//
// Concerns split:
//
//	yarilo-director hosts /api/director/...      (ring / backends / users / peers)
//	yarilo-backend-api hosts /api/backend/<service>/...  (dict / acl / quota / ...)
//
// Different binaries because director state lives in director's
// process memory and backend state (NFS + dicts) lives in backend
// pods; in multi-pod cluster these are physically separate pods, so
// they cannot be combined.
//
// Wire protocol: JSON over HTTPS. Endpoints mirror the shape of
// internal/director's existing /api/director/... surface so the
// yarilo-admin CLI can speak both with identical machinery
// (bearer-token auth, IP allow-list, application/json bodies).
//
// Streaming endpoints (currently dict/iterate) use NDJSON — one
// JSON object per line — so the CLI can pipeline display without
// buffering the entire response in memory.
package backendapi

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/authclient"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/dict"
	"github.com/0kaba0hub/yarilo/pkg/ftsproto"
	"github.com/0kaba0hub/yarilo/pkg/locks"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// Server is the backend-API HTTP server. Construct with New, then call
// Serve (mTLS-terminated TLS listener) or ListenAndServe (plain).
type Server struct {
	opts Options
	mux  *http.ServeMux

	driverCacheMu sync.Mutex
	driverCache   map[string]mailbox.MailboxBackend
}

// Options configures Server.
//
// Dicts is the live map opened by the host process; the server
// hands out pointers to those objects via lookups keyed by name.
// Mailbox / Index / Resolver / NamespaceMailboxes / Locker /
// SpecialUseDefaults / MetadataDict / Namespaces are the same shape
// session binaries already accept — backend-api opens per-user
// handles on demand from those (see userctx.go) so admin requests
// reach the same on-disk layout the IMAP path uses.
//
// Token is the shared admin secret; empty disables auth (test/dev
// only). AllowedNets, when non-empty, restricts which client IPs may
// reach the API.
type Options struct {
	Addr        string
	TLSConfig   *tls.Config
	Token       string
	AllowedNets []*net.IPNet
	Dicts       map[string]dict.Dict

	Mailbox            mailbox.MailboxBackend
	Index              mailbox.IndexBackend
	Resolver           *mailbox.Resolver
	NamespaceMailboxes map[string]mailbox.MailboxBackend
	Namespaces         []config.NamespaceConfig
	Locker             locks.Locker
	SpecialUseDefaults map[string]string
	MetadataDict       dict.Dict
	QuotaDict          dict.Dict
	// QuotaCloneDicts names the dicts (subset of Dicts) that quota_clone mirrors
	// usage into. Exposed read-only via /api/backend/quota/clone/* for operators
	// to inspect the fan-out. From cfg.Quota.CloneDicts.
	QuotaCloneDicts []string

	// AnvilAddr / AnvilTLS configure backend-api's connection to
	// yarilo-anvil for the WHO endpoint. Empty Addr disables /who
	// (returns 501). Same TLS config the rest of internal cluster
	// traffic uses.
	AnvilAddr string
	AnvilTLS  *tls.Config

	// PodIP is this backend pod's IP (from the POD_IP env / status.podIP).
	// /who scopes to sessions routed to THIS backend by default (#814),
	// matching each anvil session's Backend field against it. Empty (env not
	// injected) disables the scoping — /who then behaves as if --all was set.
	PodIP string

	// AuthClient is the live yarilo-auth master-protocol client,
	// dialled at startup from BackendAPIConfig.AuthMasterAddr.
	// When nil, /api/backend/user/info skips the userdb-enrichment
	// block and /api/backend/user/iterate returns 503. main.go
	// owns the lifecycle (Close on shutdown); the Server only
	// reads through it.
	AuthClient *authclient.Client

	// MailboxByDriver, when set, constructs a MailboxBackend for a
	// named driver (e.g. "mdbox", "sdbox"). Used when a per-user
	// mail_location specifies a different driver than the global
	// opts.Mailbox. Backends are cached internally; the factory is
	// only called once per distinct driver string.
	MailboxByDriver func(driver string) mailbox.MailboxBackend

	// FTSClient dials the yarilo-fts service for the operator fts surface
	// (status / rescan / optimize). Nil disables those endpoints (501).
	FTSClient ftsproto.Client
}

// mailboxForDriver returns the MailboxBackend for driver, using opts.Mailbox
// when driver matches the global or when no factory is configured.
// Results are cached so the factory is called at most once per driver.
func (s *Server) mailboxForDriver(driver string) mailbox.MailboxBackend {
	if driver == "" || s.opts.MailboxByDriver == nil {
		return s.opts.Mailbox
	}
	s.driverCacheMu.Lock()
	defer s.driverCacheMu.Unlock()
	if mb, ok := s.driverCache[driver]; ok {
		return mb
	}
	mb := s.opts.MailboxByDriver(driver)
	if mb == nil {
		return s.opts.Mailbox
	}
	if s.driverCache == nil {
		s.driverCache = make(map[string]mailbox.MailboxBackend)
	}
	s.driverCache[driver] = mb
	return mb
}

// New constructs a Server and registers the backend endpoints onto an
// internal ServeMux. Subsequent Server.routes additions (for ACL,
// quota, etc.) happen via dedicated wire registration in their own
// files (acl.go, quota.go); dict.go owns the dict surface.
func New(opts Options) *Server {
	s := &Server{opts: opts, mux: http.NewServeMux()}
	s.registerHealth()
	s.registerDictRoutes()
	s.registerFolderRoutes()
	s.registerUserRoutes()
	s.registerIndexRoutes()
	s.registerMdboxRoutes()
	s.registerSubscriptionRoutes()
	s.registerSpecialUseRoutes()
	s.registerMetadataRoutes()
	s.registerACLRoutes()
	s.registerQuotaRoutes()
	s.registerWhoRoutes()
	s.registerSessionRoutes()
	s.registerFTSRoutes()
	return s
}

// Serve starts the HTTP server. When opts.TLSConfig is non-nil the
// listener is TLS-terminated (use this in k8s deployments); plain
// TCP is for local dev. Blocks until ctx is cancelled or the
// underlying server returns.
func (s *Server) Serve(ctx context.Context) error {
	if s.opts.Addr == "" {
		return fmt.Errorf("backendapi: Addr is required")
	}
	srv := &http.Server{
		Addr:              s.opts.Addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		TLSConfig:         s.opts.TLSConfig,
	}
	errCh := make(chan error, 1)
	go func() {
		if s.opts.TLSConfig != nil {
			errCh <- srv.ListenAndServeTLS("", "")
		} else {
			errCh <- srv.ListenAndServe()
		}
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Handler returns the underlying mux — useful for tests that drive
// the server via httptest.Server without spinning up Serve.
func (s *Server) Handler() http.Handler { return s.mux }

// registerHealth wires GET /api/backend/health (200 if process is up).
// No auth — must be reachable by Kubernetes liveness/readiness probes.
func (s *Server) registerHealth() {
	s.mux.HandleFunc("GET /api/backend/health", func(w http.ResponseWriter, _ *http.Request) {
		apiJSON(w, map[string]string{"status": "ok"})
	})
}

// middleware chains the IP allow-list and bearer-token checks. Use
// for every authenticated endpoint. Anonymous endpoints (none yet)
// should bypass and document why.
func (s *Server) middleware(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.opts.AllowedNets) > 0 {
			clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
			ip := net.ParseIP(clientIP)
			allowed := false
			for _, n := range s.opts.AllowedNets {
				if ip != nil && n.Contains(ip) {
					allowed = true
					break
				}
			}
			if !allowed {
				apiError(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		if s.opts.Token != "" {
			auth := r.Header.Get("Authorization")
			token := strings.TrimPrefix(auth, "Bearer ")
			if !strings.HasPrefix(auth, "Bearer ") || subtle.ConstantTimeCompare([]byte(token), []byte(s.opts.Token)) != 1 {
				apiError(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	})
}

// apiJSON writes v as the response body with Content-Type
// application/json. Encoder failures are swallowed because the
// connection is already half-written; logging there would not help
// the caller. Use apiError instead for status-coded JSON errors.
func apiJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("backendapi: encode response", "err", err)
	}
}

// apiError writes a JSON {"error": msg} body with the supplied
// HTTP status. The status is set BEFORE the body so middleware
// (logger, metrics) can pick the right code.
func apiError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// decodeJSON reads r.Body into v with a 1 MiB cap; calls
// apiError on parse failure. Returns false to abort the handler.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		apiError(w, "read body: "+err.Error(), http.StatusBadRequest)
		return false
	}
	if len(body) == 0 {
		apiError(w, "request body required", http.StatusBadRequest)
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		apiError(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}
