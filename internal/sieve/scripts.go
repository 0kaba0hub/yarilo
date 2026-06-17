package sieve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/locks"
)

const (
	sieveExt       = ".sieve"
	sieveLockTTL   = 5 * time.Second
	sieveLockRenew = 2 * time.Second

	// FallbackDefaultName is used when SieveConfig.DefaultName is not set.
	FallbackDefaultName = "yarilo"
)

// ScriptStore manages per-user Sieve script files stored in the user's home
// directory. All files are hidden (dot-prefixed), matching Dovecot's convention.
//
// Layout:
//
//	%h/.<DefaultName>.sieve  — active-script pointer (symlink → .<name>.sieve, or regular file with DefaultScriptBody)
//	%h/.<name>.sieve         — named user scripts
//	%h/.yarilo.sieve-vacation — vacation dedup state
type ScriptStore struct {
	// DefaultName is the reserved script name (configured via sieve.default_name).
	// Cannot be used in PUTSCRIPT/DELETESCRIPT/RENAMESCRIPT/SETACTIVE.
	DefaultName string
	// Locker provides cross-process write coordination. Nil = single-process (tests).
	Locker locks.Locker
}

func (ss *ScriptStore) activeFile() string {
	return "." + ss.DefaultName + sieveExt
}

func (ss *ScriptStore) activePath(homeDir string) string {
	return filepath.Join(homeDir, ss.activeFile())
}

func (ss *ScriptStore) namedPath(homeDir, name string) string {
	return filepath.Join(homeDir, "."+name+sieveExt)
}

func (ss *ScriptStore) withLock(ctx context.Context, homeDir string, fn func(context.Context) error) error {
	return withSieveLock(ctx, ss.Locker, homeDir, fn)
}

// withSieveLock acquires a per-homeDir Sieve write lock and runs fn.
// When l is nil (tests, single-process) fn runs without locking.
func withSieveLock(ctx context.Context, l locks.Locker, homeDir string, fn func(context.Context) error) error {
	if l == nil {
		return fn(ctx)
	}
	return locks.WithLock(ctx, l, "sieve:"+homeDir, lockOwner(), sieveLockTTL, sieveLockRenew, fn)
}

// ActiveScriptName returns the name of the currently active script.
// If the active pointer is a symlink → returns the target name (without .sieve).
// If it is a regular file (default keep script) → returns "".
// If it does not exist → returns "".
func (ss *ScriptStore) ActiveScriptName(homeDir string) (string, error) {
	fi, err := os.Lstat(ss.activePath(homeDir))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("sieve/scripts: lstat active: %w", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return "", nil
	}
	target, err := os.Readlink(ss.activePath(homeDir))
	if err != nil {
		return "", fmt.Errorf("sieve/scripts: readlink active: %w", err)
	}
	base := strings.TrimPrefix(filepath.Base(target), ".")
	return strings.TrimSuffix(base, sieveExt), nil
}

// LoadActiveScript reads the active-script pointer (follows symlink if needed).
// Returns (nil, "", nil) when no active script file exists.
func (ss *ScriptStore) LoadActiveScript(homeDir string) (src []byte, name string, err error) {
	src, err = os.ReadFile(ss.activePath(homeDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("sieve/scripts: read active: %w", err)
	}
	name, err = ss.ActiveScriptName(homeDir)
	return src, name, err
}

// InitUser creates the active-script pointer as a regular keep file if it
// does not yet exist.
func (ss *ScriptStore) InitUser(ctx context.Context, homeDir string) error {
	if _, err := os.Lstat(ss.activePath(homeDir)); err == nil {
		return nil
	}
	return ss.withLock(ctx, homeDir, func(ctx context.Context) error {
		if _, err := os.Lstat(ss.activePath(homeDir)); err == nil {
			return nil
		}
		if err := os.MkdirAll(homeDir, 0o700); err != nil {
			return fmt.Errorf("sieve/scripts: mkdir: %w", err)
		}
		return os.WriteFile(ss.activePath(homeDir), []byte(DefaultScriptBody), 0o600)
	})
}

// SaveScript writes src to %h/.<name>.sieve atomically.
// Returns an error when name == DefaultName.
func (ss *ScriptStore) SaveScript(ctx context.Context, homeDir, name string, src []byte) error {
	if name == ss.DefaultName {
		return fmt.Errorf("sieve/scripts: %q is a reserved script name", name)
	}
	return ss.withLock(ctx, homeDir, func(ctx context.Context) error {
		target := ss.namedPath(homeDir, name)
		tmp := target + ".tmp"
		if err := os.MkdirAll(homeDir, 0o700); err != nil {
			return fmt.Errorf("sieve/scripts: mkdir: %w", err)
		}
		if err := os.WriteFile(tmp, src, 0o600); err != nil {
			return fmt.Errorf("sieve/scripts: write %q: %w", name, err)
		}
		return os.Rename(tmp, target)
	})
}

// SetActive makes the active pointer a symlink to %h/.<name>.sieve.
// Returns an error when name == DefaultName.
func (ss *ScriptStore) SetActive(ctx context.Context, homeDir, name string) error {
	if name == ss.DefaultName {
		return fmt.Errorf("sieve/scripts: %q is a reserved script name", name)
	}
	return ss.withLock(ctx, homeDir, func(ctx context.Context) error {
		link := ss.activePath(homeDir)
		tmp := link + ".tmp"
		os.Remove(tmp) //nolint:errcheck
		if err := os.Symlink("."+name+sieveExt, tmp); err != nil {
			return fmt.Errorf("sieve/scripts: symlink: %w", err)
		}
		return os.Rename(tmp, link)
	})
}

// Deactivate removes the active pointer so no named script is active.
// The next delivery will call InitUser which recreates the default keep file.
func (ss *ScriptStore) Deactivate(ctx context.Context, homeDir string) error {
	return ss.withLock(ctx, homeDir, func(ctx context.Context) error {
		err := os.Remove(ss.activePath(homeDir))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	})
}

// DeleteScript removes %h/.<name>.sieve.
// Returns an error when name == DefaultName.
func (ss *ScriptStore) DeleteScript(ctx context.Context, homeDir, name string) error {
	if name == ss.DefaultName {
		return fmt.Errorf("sieve/scripts: %q is a reserved script name", name)
	}
	return ss.withLock(ctx, homeDir, func(ctx context.Context) error {
		err := os.Remove(ss.namedPath(homeDir, name))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	})
}

// GetScript reads %h/.<name>.sieve. Returns (nil, false, nil) when not found.
func (ss *ScriptStore) GetScript(homeDir, name string) ([]byte, bool, error) {
	src, err := os.ReadFile(ss.namedPath(homeDir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("sieve/scripts: read %q: %w", name, err)
	}
	return src, true, nil
}

// ListScripts returns names of all named scripts in homeDir.
// Excludes the active-pointer file (.<DefaultName>.sieve).
func (ss *ScriptStore) ListScripts(homeDir string) ([]string, error) {
	entries, err := os.ReadDir(homeDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sieve/scripts: readdir: %w", err)
	}
	active := ss.activeFile()
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !strings.HasPrefix(n, ".") || !strings.HasSuffix(n, sieveExt) {
			continue
		}
		if n == active {
			continue
		}
		names = append(names, n[1:len(n)-len(sieveExt)])
	}
	return names, nil
}

// RenameScript renames a script and updates the active pointer if needed.
// Both oldName and newName must not be DefaultName.
func (ss *ScriptStore) RenameScript(ctx context.Context, homeDir, oldName, newName string) error {
	if oldName == ss.DefaultName || newName == ss.DefaultName {
		return fmt.Errorf("sieve/scripts: %q is a reserved script name", ss.DefaultName)
	}
	return ss.withLock(ctx, homeDir, func(ctx context.Context) error {
		if err := os.Rename(ss.namedPath(homeDir, oldName), ss.namedPath(homeDir, newName)); err != nil {
			return fmt.Errorf("sieve/scripts: rename: %w", err)
		}
		active, err := ss.ActiveScriptName(homeDir)
		if err != nil || active != oldName {
			return err
		}
		link := ss.activePath(homeDir)
		tmp := link + ".tmp"
		os.Remove(tmp) //nolint:errcheck
		if err := os.Symlink("."+newName+sieveExt, tmp); err != nil {
			return fmt.Errorf("sieve/scripts: symlink rename: %w", err)
		}
		return os.Rename(tmp, link)
	})
}

func lockOwner() string {
	return fmt.Sprintf("sieve:%d", os.Getpid())
}
