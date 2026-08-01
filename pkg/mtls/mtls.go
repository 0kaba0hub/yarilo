// Package mtls provides helpers for building mutual-TLS configs used between
// yarilo components (auth, warden, director, imap-login → imap, etc.).
// All inter-component connections use TLS 1.3 with client-cert verification.
package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sync"
	"time"
)

// ServerConfig returns a *tls.Config for mTLS servers.
// The server presents certFile/keyFile and requires clients to present a cert
// signed by the CA in caFile.
func ServerConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: load server cert %q: %w", certFile, err)
	}
	ca, err := loadCA(caFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    ca,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// ClientConfig returns a *tls.Config for mTLS clients, presenting
// certFile/keyFile and verifying the server against caFile with serverName
// pinned. serverName is required: internal services are dialled by short name,
// FQDN or pod IP interchangeably, so the pinned name (a SAN in the shared
// internal cert) is the only reliable check. cacheSize bounds the session LRU
// (negative disables resumption, 0 = default); cacheTTLSecs > 0 also expires
// cached sessions by age. Both come from internal_tls.session_cache_size /
// session_cache_ttl.
func ClientConfig(certFile, keyFile, caFile, serverName string, cacheSize, cacheTTLSecs int) (*tls.Config, error) {
	if serverName == "" {
		return nil, fmt.Errorf("mtls: empty ServerName — set internal_tls.server_name (or ring_tls_server_name for the director ring); mTLS peer verification needs a stable pinned name")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: load client cert %q: %w", certFile, err)
	}
	ca, err := loadCA(caFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      ca,
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS13,
		// Cache TLS 1.3 session tickets so repeated dials to the same peer
		// resume instead of paying a full handshake. Resumption is bound to
		// the original mutually-authenticated session, so mTLS peer identity
		// is preserved; a stale ticket falls back to a full handshake.
		ClientSessionCache: newSessionCache(cacheSize, cacheTTLSecs),
	}, nil
}

// defaultSessionCacheSize is used when internal_tls.session_cache_size is unset.
const defaultSessionCacheSize = 64

// newSessionCache builds the client session cache. size < 0 disables
// resumption; size == 0 uses the default; ttlSecs > 0 also expires sessions by age.
func newSessionCache(size, ttlSecs int) tls.ClientSessionCache {
	if size < 0 {
		return nil // resumption disabled
	}
	if size == 0 {
		size = defaultSessionCacheSize
	}
	lru := tls.NewLRUClientSessionCache(size)
	if ttlSecs <= 0 {
		return lru
	}
	return &ttlSessionCache{lru: lru, ttl: time.Duration(ttlSecs) * time.Second, put: map[string]time.Time{}}
}

// ttlSessionCache wraps an LRU tls.ClientSessionCache with a per-entry age
// limit; the stdlib LRU evicts by count only.
type ttlSessionCache struct {
	lru tls.ClientSessionCache
	ttl time.Duration

	mu  sync.Mutex
	put map[string]time.Time
}

func (c *ttlSessionCache) Put(key string, cs *tls.ClientSessionState) {
	c.mu.Lock()
	if cs == nil {
		delete(c.put, key)
	} else {
		now := time.Now()
		// The LRU evicts by count with no callback, which would leak
		// put-times; sweep TTL-expired entries here.
		for k, t := range c.put {
			if now.Sub(t) > c.ttl {
				delete(c.put, k)
			}
		}
		c.put[key] = now
	}
	c.mu.Unlock()
	c.lru.Put(key, cs)
}

func (c *ttlSessionCache) Get(key string) (*tls.ClientSessionState, bool) {
	c.mu.Lock()
	t, ok := c.put[key]
	expired := ok && time.Since(t) > c.ttl
	if expired {
		delete(c.put, key)
	}
	c.mu.Unlock()
	if expired {
		c.lru.Put(key, nil) // evict the stale session from the LRU too
		return nil, false
	}
	return c.lru.Get(key)
}

func loadCA(caFile string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: read CA %q: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("mtls: no valid CA certs in %q", caFile)
	}
	return pool, nil
}
