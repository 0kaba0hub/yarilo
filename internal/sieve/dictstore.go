package sieve

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/0kaba0hub/yarilo/pkg/dict"
	"github.com/0kaba0hub/yarilo/pkg/locks"
)

// DictScriptStore stores Sieve scripts and vacation dedup state in a dict
// backend (Redis). Keys are scoped per user:
//
//	priv/<username>/sieve/active           — name of the active script ("" = none)
//	priv/<username>/sieve/script/<name>    — script source bytes
//	priv/<username>/sieve/vacation/<handle>/<sender> — sent marker (TTL-based)
type DictScriptStore struct {
	defaultName string
	locker      locks.Locker
	d           dict.Dict
}

func (ds *DictScriptStore) DefaultScriptName() string { return ds.defaultName }

func (ds *DictScriptStore) opSet(username string) *dict.OpSettings {
	return &dict.OpSettings{Username: username}
}

func (ds *DictScriptStore) keyActive(username string) string {
	return dict.PathPrivate + dict.Escape(username) + "/sieve/active"
}

func (ds *DictScriptStore) keyScript(username, name string) string {
	return dict.PathPrivate + dict.Escape(username) + "/sieve/script/" + dict.Escape(name)
}

func (ds *DictScriptStore) keyVacation(username, handle, sender string) string {
	return dict.PathPrivate + dict.Escape(username) + "/sieve/vacation/" +
		dict.Escape(handle) + "/" + dict.Escape(sender)
}

func (ds *DictScriptStore) keyScriptPrefix(username string) string {
	return dict.PathPrivate + dict.Escape(username) + "/sieve/script/"
}

func (ds *DictScriptStore) InitUser(_ context.Context, _, _ string) error {
	return nil
}

func (ds *DictScriptStore) ActiveScriptName(ctx context.Context, username, _ string) (string, error) {
	vals, found, err := ds.d.Lookup(ctx, ds.opSet(username), ds.keyActive(username))
	if err != nil {
		return "", fmt.Errorf("sieve/dict: lookup active: %w", err)
	}
	if !found || len(vals[0]) == 0 {
		return "", nil
	}
	return string(vals[0]), nil
}

func (ds *DictScriptStore) LoadActiveScript(ctx context.Context, username, homeDir string) ([]byte, string, error) {
	name, err := ds.ActiveScriptName(ctx, username, homeDir)
	if err != nil {
		return nil, "", err
	}
	if name == "" {
		return nil, "", nil
	}
	src, found, err := ds.GetScript(ctx, username, homeDir, name)
	if err != nil {
		return nil, "", err
	}
	if !found {
		return nil, "", nil
	}
	return src, name, nil
}

func (ds *DictScriptStore) SaveScript(ctx context.Context, username, _, name string, src []byte) error {
	if name == ds.defaultName {
		return fmt.Errorf("sieve/dict: %q is a reserved script name", name)
	}
	tx, err := ds.d.Begin(ctx, ds.opSet(username))
	if err != nil {
		return fmt.Errorf("sieve/dict: begin: %w", err)
	}
	if err := tx.Set(ds.keyScript(username, name), src); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("sieve/dict: set script: %w", err)
	}
	if res, err := tx.Commit(); err != nil || res != dict.CommitOK {
		return fmt.Errorf("sieve/dict: commit save: %w", commitErr(res, err))
	}
	return nil
}

func (ds *DictScriptStore) GetScript(ctx context.Context, username, _, name string) ([]byte, bool, error) {
	vals, found, err := ds.d.Lookup(ctx, ds.opSet(username), ds.keyScript(username, name))
	if err != nil {
		return nil, false, fmt.Errorf("sieve/dict: get script: %w", err)
	}
	if !found {
		return nil, false, nil
	}
	return vals[0], true, nil
}

func (ds *DictScriptStore) ListScripts(ctx context.Context, username, _ string) ([]string, error) {
	prefix := ds.keyScriptPrefix(username)
	iter, err := ds.d.Iterate(ctx, ds.opSet(username), prefix, dict.IterNoValue)
	if err != nil {
		return nil, fmt.Errorf("sieve/dict: iterate scripts: %w", err)
	}
	defer iter.Close() //nolint:errcheck
	var names []string
	for iter.Next() {
		k := iter.Key()
		name := strings.TrimPrefix(k, prefix)
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		names = append(names, dict.Unescape(name))
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("sieve/dict: iterate scripts: %w", err)
	}
	return names, nil
}

func (ds *DictScriptStore) SetActive(ctx context.Context, username, _, name string) error {
	if name == ds.defaultName {
		return fmt.Errorf("sieve/dict: %q is a reserved script name", name)
	}
	tx, err := ds.d.Begin(ctx, ds.opSet(username))
	if err != nil {
		return fmt.Errorf("sieve/dict: begin: %w", err)
	}
	if err := tx.Set(ds.keyActive(username), []byte(name)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("sieve/dict: set active: %w", err)
	}
	if res, err := tx.Commit(); err != nil || res != dict.CommitOK {
		return fmt.Errorf("sieve/dict: commit active: %w", commitErr(res, err))
	}
	return nil
}

func (ds *DictScriptStore) Deactivate(ctx context.Context, username, _ string) error {
	tx, err := ds.d.Begin(ctx, ds.opSet(username))
	if err != nil {
		return fmt.Errorf("sieve/dict: begin: %w", err)
	}
	if err := tx.Unset(ds.keyActive(username)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("sieve/dict: unset active: %w", err)
	}
	if res, err := tx.Commit(); err != nil || res != dict.CommitOK {
		return fmt.Errorf("sieve/dict: commit deactivate: %w", commitErr(res, err))
	}
	return nil
}

func (ds *DictScriptStore) DeleteScript(ctx context.Context, username, _, name string) error {
	if name == ds.defaultName {
		return fmt.Errorf("sieve/dict: %q is a reserved script name", name)
	}
	tx, err := ds.d.Begin(ctx, ds.opSet(username))
	if err != nil {
		return fmt.Errorf("sieve/dict: begin: %w", err)
	}
	if err := tx.Unset(ds.keyScript(username, name)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("sieve/dict: unset script: %w", err)
	}
	if res, err := tx.Commit(); err != nil || res != dict.CommitOK {
		return fmt.Errorf("sieve/dict: commit delete: %w", commitErr(res, err))
	}
	return nil
}

func (ds *DictScriptStore) RenameScript(ctx context.Context, username, homeDir, oldName, newName string) error {
	if oldName == ds.defaultName || newName == ds.defaultName {
		return fmt.Errorf("sieve/dict: %q is a reserved script name", ds.defaultName)
	}
	src, found, err := ds.GetScript(ctx, username, homeDir, oldName)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("sieve/dict: script %q not found", oldName)
	}
	active, err := ds.ActiveScriptName(ctx, username, homeDir)
	if err != nil {
		return err
	}
	tx, err := ds.d.Begin(ctx, ds.opSet(username))
	if err != nil {
		return fmt.Errorf("sieve/dict: begin: %w", err)
	}
	if err := tx.Set(ds.keyScript(username, newName), src); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("sieve/dict: set new script: %w", err)
	}
	if err := tx.Unset(ds.keyScript(username, oldName)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("sieve/dict: unset old script: %w", err)
	}
	if active == oldName {
		if err := tx.Set(ds.keyActive(username), []byte(newName)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("sieve/dict: update active: %w", err)
		}
	}
	if res, err := tx.Commit(); err != nil || res != dict.CommitOK {
		return fmt.Errorf("sieve/dict: commit rename: %w", commitErr(res, err))
	}
	return nil
}

func (ds *DictScriptStore) VacationSent(ctx context.Context, username, _, handle, senderAddr string) (bool, error) {
	key := ds.keyVacation(username, handle, senderAddr)
	_, found, err := ds.d.Lookup(ctx, ds.opSet(username), key)
	if err != nil {
		return false, fmt.Errorf("sieve/dict: vacation lookup: %w", err)
	}
	return found, nil
}

func (ds *DictScriptStore) MarkVacationSent(ctx context.Context, username, _ string, handle, senderAddr string, ttlSecs int) error {
	op := &dict.OpSettings{Username: username, ExpireSecs: uint32(ttlSecs)} //nolint:gosec
	tx, err := ds.d.Begin(ctx, op)
	if err != nil {
		return fmt.Errorf("sieve/dict: begin vacation: %w", err)
	}
	if err := tx.Set(ds.keyVacation(username, handle, senderAddr), []byte("1")); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("sieve/dict: set vacation: %w", err)
	}
	if res, err := tx.Commit(); err != nil || res != dict.CommitOK {
		return fmt.Errorf("sieve/dict: commit vacation: %w", commitErr(res, err))
	}
	return nil
}

func commitErr(res dict.CommitResult, err error) error {
	if err != nil {
		return err
	}
	switch res {
	case dict.CommitNotFound:
		return errors.New("key not found")
	case dict.CommitFailed:
		return errors.New("commit failed")
	case dict.CommitWriteUncertain:
		return errors.New("write uncertain")
	}
	return nil
}
