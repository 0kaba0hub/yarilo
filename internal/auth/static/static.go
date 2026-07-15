// Package static implements a static passdb and userdb: one shared credential
// and a set of templated fields applied to every user. Useful for tests,
// single-mailbox installs, and proxy front-ends where the backend performs the
// real authentication. It matches every username, so in a chain it belongs
// last.
package static

import (
	"fmt"
	"strings"

	"github.com/emersion/go-sasl"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	"github.com/0kaba0hub/yarilo/internal/auth/scheme"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// userdbFieldPrefix marks a template field as userdb-only (same convention as
// the passwd-file extra column).
const userdbFieldPrefix = "userdb_"

// Config is the open-time configuration for a static backend.
type Config struct {
	// Password is the shared credential ({SCHEME} prefix or DefaultScheme).
	// Empty requires Nopassword.
	Password string
	// Nopassword accepts any supplied password — for proxy front-ends where the
	// upstream authenticates. Mutually exclusive with a non-empty Password.
	Nopassword bool
	// DefaultScheme is the assumed scheme when Password carries no {SCHEME}
	// prefix and no crypt(3) marker. Empty defaults to PLAIN.
	DefaultScheme string
	// Fields are templated user fields (%u/%n/%d expanded per lookup).
	// userdb_-prefixed keys populate the userdb; bare keys are forwarded on the
	// passdb path (allow_nets, proxy, ...).
	Fields map[string]string
}

// DB is a static passdb + userdb.
type DB struct {
	password      string
	nopassword    bool
	defaultScheme string
	fields        map[string]string
}

// New validates and builds a static backend.
func New(c Config) (*DB, error) {
	if c.Password == "" && !c.Nopassword {
		return nil, fmt.Errorf("auth/static: static_password is empty and nopassword is not set")
	}
	if c.Password != "" && c.Nopassword {
		return nil, fmt.Errorf("auth/static: static_password and nopassword are mutually exclusive")
	}
	return &DB{
		password:      c.Password,
		nopassword:    c.Nopassword,
		defaultScheme: c.DefaultScheme,
		fields:        c.Fields,
	}, nil
}

// Authenticate implements protocol.Passdb. Static matches every username, so it
// never returns ResultNext: a mismatch is a definitive ResultFail.
func (db *DB) Authenticate(req *protocol.Request) (protocol.Result, error) {
	if !db.nopassword && !scheme.VerifyWithDefault(db.password, req.Password, db.defaultScheme) {
		return protocol.ResultFail, nil
	}
	req.Fields.Set("user", req.Username)
	for k, v := range db.fields {
		if v == "" || strings.HasPrefix(k, userdbFieldPrefix) {
			continue
		}
		req.Fields.Set(k, mailbox.ExpandVars(v, req.Username))
	}
	return protocol.ResultOK, nil
}

// Lookup implements protocol.Userdb. Static resolves every username, rendering
// the userdb_-prefixed template fields for the given user.
func (db *DB) Lookup(username string) (*protocol.UserInfo, error) {
	info := &protocol.UserInfo{Username: username}
	for k, v := range db.fields {
		if v == "" {
			continue
		}
		key, ok := strings.CutPrefix(k, userdbFieldPrefix)
		if !ok {
			continue
		}
		if err := protocol.AssignField(info, key, mailbox.ExpandVars(v, username)); err != nil {
			return nil, fmt.Errorf("auth/static: userdb field %q: %w", key, err)
		}
	}
	return info, nil
}

// LookupSCRAMSha256 satisfies protocol.SCRAMSha256Lookup. When the shared
// credential is a {SCRAM-SHA-256} verifier it is returned for every user;
// otherwise (nil, nil) so the SASL mech fabricates a fake verifier.
func (db *DB) LookupSCRAMSha256(string) (*sasl.ScramCredentials, error) {
	return db.scram(scheme.ParseSCRAMSha256Credentials), nil
}

// LookupSCRAMSha1 is the SHA-1 counterpart.
func (db *DB) LookupSCRAMSha1(string) (*sasl.ScramCredentials, error) {
	return db.scram(scheme.ParseSCRAMSha1Credentials), nil
}

func (db *DB) scram(parse func(string) (*sasl.ScramCredentials, bool)) *sasl.ScramCredentials {
	if db.password == "" {
		return nil
	}
	creds, ok := parse(db.password)
	if !ok {
		return nil
	}
	return creds
}
