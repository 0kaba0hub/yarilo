// Package dkim provides DKIM verification and signing with pluggable key backends.
// Signing supports static (config file) and dynamic (SQL database) key sources.
package dkim

import (
	"context"
	"crypto"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	goDKIM "github.com/emersion/go-msgauth/dkim"

	// SQL drivers registered here; mysql/postgres registered by callers if needed.
	_ "modernc.org/sqlite" // SQLite driver (no cgo)
)

// Result summarises a single DKIM signature verification.
type Result struct {
	Domain string
	Pass   bool
	Err    error
}

// Verify checks all DKIM-Signature headers in the message.
// r is consumed entirely. An empty slice with nil error means no signatures present.
func Verify(r io.Reader) ([]Result, error) {
	verifs, err := goDKIM.Verify(r)
	if err != nil {
		return nil, fmt.Errorf("dkim/verify: %w", err)
	}
	out := make([]Result, len(verifs))
	for i, v := range verifs {
		out[i] = Result{
			Domain: v.Domain,
			Pass:   v.Err == nil,
			Err:    v.Err,
		}
	}
	return out, nil
}

// SignConfig holds per-message signing parameters (resolved from Config).
type SignConfig struct {
	Selector        string
	SignHeaders      []string
	OversignHeaders []string
}

// Sign adds a DKIM-Signature header to the message.
// w receives the signed message (original headers + injected DKIM-Signature + body).
// Oversigning is implemented by including oversigned headers twice in h= (RFC 6376 §8.7).
func Sign(w io.Writer, r io.Reader, domain string, signer crypto.Signer, cfg SignConfig) error {
	// Build header list: regular headers first, then oversigned ones appended again.
	// Duplicate entries in h= cause verifiers to reject any injected extra header.
	keys := append([]string(nil), cfg.SignHeaders...)
	keys = append(keys, cfg.OversignHeaders...)

	opts := &goDKIM.SignOptions{
		Domain:     domain,
		Selector:   cfg.Selector,
		Signer:     signer,
		HeaderKeys: keys,
	}
	if err := goDKIM.Sign(w, r, opts); err != nil {
		return fmt.Errorf("dkim/sign %s: %w", domain, err)
	}
	return nil
}

// KeyProvider resolves a private signing key for a given domain.
type KeyProvider interface {
	GetPrivateKey(ctx context.Context, domain string) (crypto.Signer, error)
}

// ---- StaticKeyProvider --------------------------------------------------

// StaticKeyProvider loads keys from PEM files specified in config.
// keys maps domain → file path.
type StaticKeyProvider struct {
	mu    sync.RWMutex
	cache map[string]crypto.Signer
	paths map[string]string
}

// NewStaticKeyProvider creates a StaticKeyProvider.
// paths maps domain name → path to RSA/Ed25519 PEM private key file.
func NewStaticKeyProvider(paths map[string]string) *StaticKeyProvider {
	return &StaticKeyProvider{
		cache: make(map[string]crypto.Signer, len(paths)),
		paths: paths,
	}
}

func (p *StaticKeyProvider) GetPrivateKey(_ context.Context, domain string) (crypto.Signer, error) {
	p.mu.RLock()
	if k, ok := p.cache[domain]; ok {
		p.mu.RUnlock()
		return k, nil
	}
	p.mu.RUnlock()

	path, ok := p.paths[domain]
	if !ok {
		return nil, fmt.Errorf("dkim/static: no key configured for domain %q", domain)
	}
	k, err := loadPEMKey(path)
	if err != nil {
		return nil, fmt.Errorf("dkim/static: %w", err)
	}

	p.mu.Lock()
	p.cache[domain] = k
	p.mu.Unlock()
	return k, nil
}

// ---- SQLKeyProvider -----------------------------------------------------

type cachedKey struct {
	key     crypto.Signer
	expires time.Time
}

// SQLKeyProvider fetches private keys from a SQL database with an in-memory cache.
// The query must accept one parameter (the domain) and return a single column: the PEM private key.
// Placeholder is chosen automatically: $1 for postgres, ? for sqlite/mysql.
type SQLKeyProvider struct {
	db       *sql.DB
	query    string
	cacheTTL time.Duration

	mu    sync.Mutex
	cache map[string]cachedKey
}

// NewSQLKeyProvider opens a DB connection and returns a SQLKeyProvider.
// driver is "sqlite" | "mysql" | "postgres". dsn supports ${ENV_VAR}.
func NewSQLKeyProvider(driver, dsn, query string, cacheTTL time.Duration) (*SQLKeyProvider, error) {
	dbDriver := sqlDriver(driver)
	db, err := sql.Open(dbDriver, dsn)
	if err != nil {
		return nil, fmt.Errorf("dkim/sql: open %s: %w", driver, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("dkim/sql: ping %s: %w", driver, err)
	}
	if cacheTTL == 0 {
		cacheTTL = 5 * time.Minute
	}
	// Normalise placeholder for postgres ($1 vs ?).
	q := normaliseQuery(query, driver)
	return &SQLKeyProvider{
		db:       db,
		query:    q,
		cacheTTL: cacheTTL,
		cache:    make(map[string]cachedKey),
	}, nil
}

func (p *SQLKeyProvider) GetPrivateKey(ctx context.Context, domain string) (crypto.Signer, error) {
	p.mu.Lock()
	if c, ok := p.cache[domain]; ok && time.Now().Before(c.expires) {
		p.mu.Unlock()
		return c.key, nil
	}
	p.mu.Unlock()

	var pemData string
	if err := p.db.QueryRowContext(ctx, p.query, domain).Scan(&pemData); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("dkim/sql: no key for domain %q", domain)
		}
		return nil, fmt.Errorf("dkim/sql: query for %q: %w", domain, err)
	}

	key, err := parsePEMKey([]byte(pemData))
	if err != nil {
		return nil, fmt.Errorf("dkim/sql: parse key for %q: %w", domain, err)
	}

	p.mu.Lock()
	p.cache[domain] = cachedKey{key: key, expires: time.Now().Add(p.cacheTTL)}
	p.mu.Unlock()
	return key, nil
}

func (p *SQLKeyProvider) Close() error {
	return p.db.Close()
}

// ---- helpers ------------------------------------------------------------

func loadPEMKey(path string) (crypto.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return parsePEMKey(data)
}

func parsePEMKey(data []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		s, ok := k.(crypto.Signer)
		if !ok {
			return nil, fmt.Errorf("PKCS8 key does not implement crypto.Signer")
		}
		return s, nil
	default:
		return nil, fmt.Errorf("unsupported PEM type %q", block.Type)
	}
}

func sqlDriver(driver string) string {
	switch strings.ToLower(driver) {
	case "postgres", "postgresql":
		return "postgres"
	case "mysql":
		return "mysql"
	default:
		return "sqlite3"
	}
}

// normaliseQuery replaces the generic ? placeholder with $1 for PostgreSQL.
func normaliseQuery(q, driver string) string {
	switch strings.ToLower(driver) {
	case "postgres", "postgresql":
		return strings.ReplaceAll(q, "?", "$1")
	default:
		return strings.ReplaceAll(q, "$1", "?")
	}
}
