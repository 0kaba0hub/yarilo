// Package fail is a dict driver that fails every operation.
//
// Used to wire up explicit "metadata disabled" or "ACL disabled"
// surfaces in code paths that take a dict.Dict, without having to add
// nil-checks at every call site. Also useful in unit tests that need
// to exercise driver error paths.
//
// Mirrors Dovecot's dict-fail.c.
package fail

import (
	"context"
	"errors"
	"fmt"

	"github.com/0kaba0hub/yarilo/pkg/dict"
)

func init() {
	dict.Register("fail", New)
}

// ErrFailDriver is the sentinel error every fail-driver operation
// returns. Callers can branch on it (errors.Is) if they need to treat
// "dict disabled" differently from real I/O failures.
var ErrFailDriver = errors.New("dict/fail: driver is disabled")

// New constructs a fail dict. The Config.Settings map MAY contain
// "message" (string) to customise the error text; otherwise a
// generic disabled-driver message is returned.
func New(cfg dict.Config) (dict.Dict, error) {
	msg := ""
	if m, ok := cfg.Settings["message"].(string); ok {
		msg = m
	}
	return &Dict{msg: msg}, nil
}

type Dict struct{ msg string }

func (d *Dict) Name() string                       { return "fail" }
func (d *Dict) Close() error                       { return nil }
func (d *Dict) Wait(_ context.Context) error       { return nil }
func (d *Dict) ExpireScan(_ context.Context) error { return d.err("expire-scan") }

func (d *Dict) Lookup(_ context.Context, _ *dict.OpSettings, _ string) ([][]byte, bool, error) {
	return nil, false, d.err("lookup")
}

func (d *Dict) Iterate(_ context.Context, _ *dict.OpSettings, _ string, _ dict.IterFlag) (dict.Iterator, error) {
	return nil, d.err("iterate")
}

func (d *Dict) Begin(_ context.Context, _ *dict.OpSettings) (dict.Tx, error) {
	return nil, d.err("begin")
}

func (d *Dict) err(op string) error {
	if d.msg != "" {
		return fmt.Errorf("%w: %s (op=%s)", ErrFailDriver, d.msg, op)
	}
	return fmt.Errorf("%w (op=%s)", ErrFailDriver, op)
}
