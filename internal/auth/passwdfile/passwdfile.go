// Package passwdfile implements a passwd-file passdb and userdb: a flat,
// colon-separated file of users (classic /etc/passwd layout with an optional
// extra-fields column). One file serves both roles, so a self-hosted
// deployment can run without an SQL database. The file is reloaded lazily when
// its mtime or size changes.
package passwdfile

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/emersion/go-sasl"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
	"github.com/yarilomail/yarilo/internal/auth/scheme"
)

// userdbFieldPrefix marks an extra field as userdb-only (not forwarded on the
// passdb path). Mirrors the passwd-file convention.
const userdbFieldPrefix = "userdb_"

// Config is the open-time configuration for a passwd-file backend.
type Config struct {
	// Path is the passwd-file location.
	Path string
	// DefaultScheme is the assumed password scheme when a stored password
	// carries no {SCHEME} prefix and no crypt(3) marker. Empty defaults to
	// CRYPT (crypt(3) autodetection).
	DefaultScheme string
}

// DB is a passwd-file backend. It satisfies protocol.Passdb, protocol.Userdb,
// protocol.UserdbIterator and the SCRAM lookup interfaces, so a single instance
// can be wired into both the passdb and userdb chains.
type DB struct {
	path          string
	defaultScheme string

	mu    sync.RWMutex
	mtime int64
	size  int64
	users map[string]*user
}

// New opens a passwd-file backend and loads it once so a missing or malformed
// file fails fast at startup rather than on the first login.
func New(c Config) (*DB, error) {
	if c.Path == "" {
		return nil, fmt.Errorf("auth/passwdfile: empty path")
	}
	ds := c.DefaultScheme
	if ds == "" {
		ds = "CRYPT"
	}
	db := &DB{path: c.Path, defaultScheme: ds}
	if err := db.reload(); err != nil {
		return nil, err
	}
	return db, nil
}

// reload re-reads the file when its mtime or size differs from the cached
// snapshot. The first call (mtime==0) always loads.
func (db *DB) reload() error {
	fi, err := os.Stat(db.path)
	if err != nil {
		return fmt.Errorf("auth/passwdfile: stat %s: %w", db.path, err)
	}
	mtime, size := fi.ModTime().UnixNano(), fi.Size()

	db.mu.RLock()
	fresh := db.users != nil && db.mtime == mtime && db.size == size
	db.mu.RUnlock()
	if fresh {
		return nil
	}

	body, err := os.ReadFile(db.path)
	if err != nil {
		return fmt.Errorf("auth/passwdfile: read %s: %w", db.path, err)
	}
	users := parse(string(body))

	db.mu.Lock()
	db.users, db.mtime, db.size = users, mtime, size
	db.mu.Unlock()
	return nil
}

// lookup returns the record for username, reloading the file if it changed.
func (db *DB) lookup(username string) (*user, bool) {
	if err := db.reload(); err != nil {
		slog.Warn("auth/passwdfile: reload failed, using cached snapshot", "path", db.path, "err", err)
	}
	db.mu.RLock()
	defer db.mu.RUnlock()
	u, ok := db.users[username]
	return u, ok
}

// Authenticate implements protocol.Passdb. It verifies the password against the
// stored scheme and, on success, populates req.Fields with user / home and any
// non-userdb extra fields (allow_nets, nologin, proxy, ...).
func (db *DB) Authenticate(req *protocol.Request) (protocol.Result, error) {
	u, ok := db.lookup(req.Username)
	if !ok {
		return protocol.ResultNext, nil
	}
	if u.password == "" {
		return protocol.ResultFail, nil
	}
	if !scheme.VerifyWithDefault(u.password, req.Password, db.defaultScheme) {
		return protocol.ResultFail, nil
	}

	req.Fields.Set("user", req.Username)
	if u.home != "" {
		req.Fields.Set("home", u.home)
	}
	// Forward passdb-side extra fields; userdb_-prefixed fields are for the
	// userdb path only and are not surfaced here.
	for k, v := range u.extra {
		if v == "" || strings.HasPrefix(k, userdbFieldPrefix) {
			continue
		}
		req.Fields.Set(k, v)
	}
	return protocol.ResultOK, nil
}

// Lookup implements protocol.Userdb. Returns (nil, nil) when the user is absent
// so a UserdbChain falls through to the next backend.
func (db *DB) Lookup(username string) (*protocol.UserInfo, error) {
	u, ok := db.lookup(username)
	if !ok {
		return nil, nil
	}
	info := &protocol.UserInfo{Username: username}
	if u.home != "" {
		if err := protocol.AssignField(info, "home", u.home); err != nil {
			return nil, fmt.Errorf("auth/passwdfile: home: %w", err)
		}
	}
	for k, v := range u.extra {
		if v == "" {
			continue
		}
		// userdb_-prefixed fields carry the userdb payload; bare keys are
		// passdb-only and ignored on this path.
		key, ok := strings.CutPrefix(k, userdbFieldPrefix)
		if !ok {
			continue
		}
		if err := protocol.AssignField(info, key, v); err != nil {
			return nil, fmt.Errorf("auth/passwdfile: userdb field %q: %w", key, err)
		}
	}
	return info, nil
}

// Iterate implements protocol.UserdbIterator: lists every username in the file.
func (db *DB) Iterate() ([]string, error) {
	if err := db.reload(); err != nil {
		return nil, err
	}
	db.mu.RLock()
	defer db.mu.RUnlock()
	out := make([]string, 0, len(db.users))
	for name := range db.users {
		out = append(out, name)
	}
	return out, nil
}

// LookupSCRAMSha256 satisfies protocol.SCRAMSha256Lookup. Returns (nil, nil)
// when the user is unknown or the stored password is not a SCRAM-SHA-256
// verifier, so the SASL mech fabricates a fake verifier and the exchange fails
// uniformly without leaking user existence.
func (db *DB) LookupSCRAMSha256(username string) (*sasl.ScramCredentials, error) {
	return db.lookupSCRAM(username, scheme.ParseSCRAMSha256Credentials), nil
}

// LookupSCRAMSha1 is the SHA-1 counterpart of LookupSCRAMSha256.
func (db *DB) LookupSCRAMSha1(username string) (*sasl.ScramCredentials, error) {
	return db.lookupSCRAM(username, scheme.ParseSCRAMSha1Credentials), nil
}

func (db *DB) lookupSCRAM(username string, parse func(string) (*sasl.ScramCredentials, bool)) *sasl.ScramCredentials {
	u, ok := db.lookup(username)
	if !ok || u.password == "" {
		return nil
	}
	creds, ok := parse(u.password)
	if !ok {
		return nil
	}
	return creds
}

// DriverName satisfies protocol.DriverName for passdb metrics.
func (db *DB) DriverName() string { return "passwd-file" }
