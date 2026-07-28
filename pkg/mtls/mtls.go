// Package mtls provides helpers for building mutual-TLS configs used between
// yarilo components (auth, anvil, director, imap-login → imap, etc.).
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

// ClientConfig returns a *tls.Config for mTLS clients. The client presents
// certFile/keyFile and verifies the server against caFile, pinning serverName
// as the expected certificate name (#816). serverName is REQUIRED: internal
// services are dialled by short name / FQDN / pod IP interchangeably, so
// verifying against the dialed host is unreliable — the pinned name must be a
// SAN in the shared internal cert. An empty serverName is a misconfiguration
// (typically an empty internal_tls.server_name) and fails loudly here rather
// than as a cryptic "ServerName must be specified" on the first dial.
// cacheSize bounds the client session LRU (0 disables resumption — every dial
// pays a full handshake). cacheTTLSecs, when > 0, expires a cached session that
// age in addition to LRU eviction; 0 means LRU-only. Both come from
// internal_tls.session_cache_size / session_cache_ttl.
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
		// Cache TLS 1.3 session tickets so repeated dials to the same internal
		// peer resume (PSK) instead of paying a full handshake each time (#856).
		// Every component builds one client config per peer at startup and reuses
		// it for all dials, so the cache is shared across those dials. Resumption
		// is bound to the original mutually-authenticated session, so it preserves
		// the mTLS peer identity; a stale/unknown ticket falls back to a full
		// handshake. This cuts the delivery→verify handshake CPU that pushed the
		// heavy sieve smoke tests past their read deadline on a single-core node.
		ClientSessionCache: newSessionCache(cacheSize, cacheTTLSecs),
	}, nil
}

// defaultSessionCacheSize is used when internal_tls.session_cache_size is unset
// (koanf zero). A component dials only a handful of distinct internal peers.
const defaultSessionCacheSize = 64

// newSessionCache builds the client session cache from the configured size/TTL.
// size < 0 disables resumption; size == 0 uses the default; ttlSecs > 0 wraps
// the LRU so a cached session also expires by age (e.g. so a cert rotation
// stops resuming old sessions within the TTL).
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
// limit: a session older than ttl is treated as absent (and evicted) on Get, so
// resumption never uses a session past the operator's TTL. Go's stdlib LRU
// caches by count only.
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
		c.put[key] = time.Now()
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
