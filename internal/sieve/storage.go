package sieve

import (
	"context"
	"fmt"

	"github.com/0kaba0hub/yarilo/pkg/dict"
)

const (
	keyActive   = dict.PathPrivate + "sieve/active"
	keyScript   = dict.PathPrivate + "sieve/script/"
	keyVacation = dict.PathPrivate + "sieve/vacation/"
)

// DefaultScriptName is the name given to the initial Sieve script created for a user.
const DefaultScriptName = "yarilo.sieve"

// DefaultScriptBody is the content of the initial script (keep = deliver normally).
const DefaultScriptBody = "keep;\n"

func opSettings(username, homeDir string) *dict.OpSettings {
	return &dict.OpSettings{Username: username, HomeDir: homeDir}
}

// loadActiveScript returns the source of the currently active script and its name.
// Returns (nil, "", nil) when no active script is configured.
func loadActiveScript(ctx context.Context, d dict.Dict, username, homeDir string) (src []byte, name string, err error) {
	ops := opSettings(username, homeDir)

	vals, found, err := d.Lookup(ctx, ops, keyActive)
	if err != nil {
		return nil, "", fmt.Errorf("sieve/storage: lookup active: %w", err)
	}
	if !found || len(vals) == 0 {
		return nil, "", nil
	}
	name = string(vals[0])

	vals, found, err = d.Lookup(ctx, ops, keyScript+dict.Escape(name))
	if err != nil {
		return nil, "", fmt.Errorf("sieve/storage: lookup script %q: %w", name, err)
	}
	if !found || len(vals) == 0 {
		return nil, "", nil
	}
	return vals[0], name, nil
}

// InitUser writes the default yarilo.sieve script and marks it active if the
// user has no active script yet. Safe to call on every delivery (no-op when
// the key already exists).
func InitUser(ctx context.Context, d dict.Dict, username, homeDir string) error {
	ops := opSettings(username, homeDir)

	_, found, err := d.Lookup(ctx, ops, keyActive)
	if err != nil {
		return fmt.Errorf("sieve/storage: init user check: %w", err)
	}
	if found {
		return nil
	}

	tx, err := d.Begin(ctx, ops)
	if err != nil {
		return fmt.Errorf("sieve/storage: begin init: %w", err)
	}
	if err := tx.Set(keyScript+dict.Escape(DefaultScriptName), []byte(DefaultScriptBody)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("sieve/storage: set default script: %w", err)
	}
	if err := tx.Set(keyActive, []byte(DefaultScriptName)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("sieve/storage: set active: %w", err)
	}
	if _, err := tx.Commit(); err != nil {
		return fmt.Errorf("sieve/storage: commit init: %w", err)
	}
	return nil
}

// SaveScript writes or overwrites a named script.
func SaveScript(ctx context.Context, d dict.Dict, username, homeDir, name string, src []byte) error {
	ops := opSettings(username, homeDir)
	tx, err := d.Begin(ctx, ops)
	if err != nil {
		return fmt.Errorf("sieve/storage: begin save: %w", err)
	}
	if err := tx.Set(keyScript+dict.Escape(name), src); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("sieve/storage: set script %q: %w", name, err)
	}
	if _, err := tx.Commit(); err != nil {
		return fmt.Errorf("sieve/storage: commit save: %w", err)
	}
	return nil
}

// SetActive marks the named script as active.
func SetActive(ctx context.Context, d dict.Dict, username, homeDir, name string) error {
	ops := opSettings(username, homeDir)
	tx, err := d.Begin(ctx, ops)
	if err != nil {
		return fmt.Errorf("sieve/storage: begin setactive: %w", err)
	}
	if err := tx.Set(keyActive, []byte(name)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("sieve/storage: set active %q: %w", name, err)
	}
	if _, err := tx.Commit(); err != nil {
		return fmt.Errorf("sieve/storage: commit setactive: %w", err)
	}
	return nil
}

// DeactivateScript removes the active-script pointer (no script = implicit keep).
func DeactivateScript(ctx context.Context, d dict.Dict, username, homeDir string) error {
	ops := opSettings(username, homeDir)
	tx, err := d.Begin(ctx, ops)
	if err != nil {
		return fmt.Errorf("sieve/storage: begin deactivate: %w", err)
	}
	if err := tx.Unset(keyActive); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("sieve/storage: unset active: %w", err)
	}
	if _, err := tx.Commit(); err != nil {
		return fmt.Errorf("sieve/storage: commit deactivate: %w", err)
	}
	return nil
}

// DeleteScript removes a named script. The caller must ensure it is not active.
func DeleteScript(ctx context.Context, d dict.Dict, username, homeDir, name string) error {
	ops := opSettings(username, homeDir)
	tx, err := d.Begin(ctx, ops)
	if err != nil {
		return fmt.Errorf("sieve/storage: begin delete: %w", err)
	}
	if err := tx.Unset(keyScript + dict.Escape(name)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("sieve/storage: unset script %q: %w", name, err)
	}
	if _, err := tx.Commit(); err != nil {
		return fmt.Errorf("sieve/storage: commit delete: %w", err)
	}
	return nil
}

// GetScript retrieves the source of a named script.
// Returns (nil, false, nil) when the script does not exist.
func GetScript(ctx context.Context, d dict.Dict, username, homeDir, name string) ([]byte, bool, error) {
	ops := opSettings(username, homeDir)
	vals, found, err := d.Lookup(ctx, ops, keyScript+dict.Escape(name))
	if err != nil || !found || len(vals) == 0 {
		return nil, found, err
	}
	return vals[0], true, nil
}

// ActiveScriptName returns the name of the currently active script, or "" if none.
func ActiveScriptName(ctx context.Context, d dict.Dict, username, homeDir string) (string, error) {
	ops := opSettings(username, homeDir)
	vals, found, err := d.Lookup(ctx, ops, keyActive)
	if err != nil {
		return "", fmt.Errorf("sieve/storage: lookup active: %w", err)
	}
	if !found || len(vals) == 0 {
		return "", nil
	}
	return string(vals[0]), nil
}

// ListScripts returns the names of all scripts stored for the user.
func ListScripts(ctx context.Context, d dict.Dict, username, homeDir string) ([]string, error) {
	ops := opSettings(username, homeDir)
	it, err := d.Iterate(ctx, ops, keyScript, dict.IterNoValue)
	if err != nil {
		return nil, fmt.Errorf("sieve/storage: iterate scripts: %w", err)
	}
	defer it.Close()

	var names []string
	for it.Next() {
		key := it.Key()
		if len(key) > len(keyScript) {
			names = append(names, dict.Unescape(key[len(keyScript):]))
		}
	}
	return names, it.Err()
}
