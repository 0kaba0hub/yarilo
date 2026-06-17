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
	activeFile     = "yarilo.sieve"
	sieveExt       = ".sieve"
	sieveLockTTL   = 5 * time.Second
	sieveLockRenew = 2 * time.Second
)

// ReservedScriptName is the script name reserved for the default active-script
// entry point. It cannot be used in PUTSCRIPT/DELETESCRIPT/RENAMESCRIPT.
const ReservedScriptName = DefaultScriptName

func activePath(homeDir string) string {
	return filepath.Join(homeDir, activeFile)
}

func namedScriptPath(homeDir, name string) string {
	return filepath.Join(homeDir, name+sieveExt)
}

func sieveLockResource(homeDir string) string { return "sieve:" + homeDir }

// withSieveLock acquires a per-homeDir Sieve write lock and runs fn.
// When l is nil (tests, single-process) fn runs without locking.
func withSieveLock(ctx context.Context, l locks.Locker, homeDir string, fn func(context.Context) error) error {
	if l == nil {
		return fn(ctx)
	}
	return locks.WithLock(ctx, l, sieveLockResource(homeDir), lockOwner(), sieveLockTTL, sieveLockRenew, fn)
}

// FsActiveScriptName returns the name of the currently active script.
// If yarilo.sieve is a symlink, returns the target name (without .sieve).
// If yarilo.sieve is a regular file, returns "" (default built-in, not a named script).
// If yarilo.sieve does not exist, returns "".
func FsActiveScriptName(homeDir string) (string, error) {
	fi, err := os.Lstat(activePath(homeDir))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("sieve/scripts: lstat active: %w", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return "", nil // regular file — default, no named script active
	}
	target, err := os.Readlink(activePath(homeDir))
	if err != nil {
		return "", fmt.Errorf("sieve/scripts: readlink active: %w", err)
	}
	return strings.TrimSuffix(filepath.Base(target), sieveExt), nil
}

// FsLoadActiveScript reads yarilo.sieve (follows symlink if needed).
// Returns (nil, "", nil) when no active script file exists.
func FsLoadActiveScript(homeDir string) (src []byte, name string, err error) {
	src, err = os.ReadFile(activePath(homeDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("sieve/scripts: read active: %w", err)
	}
	name, err = FsActiveScriptName(homeDir)
	return src, name, err
}

// FsInitUser creates yarilo.sieve as a regular keep-file if it does not yet exist.
func FsInitUser(ctx context.Context, l locks.Locker, homeDir string) error {
	if _, err := os.Lstat(activePath(homeDir)); err == nil {
		return nil
	}
	return withSieveLock(ctx, l, homeDir, func(ctx context.Context) error {
		if _, err := os.Lstat(activePath(homeDir)); err == nil {
			return nil
		}
		if err := os.MkdirAll(homeDir, 0o700); err != nil {
			return fmt.Errorf("sieve/scripts: mkdir: %w", err)
		}
		return os.WriteFile(activePath(homeDir), []byte(DefaultScriptBody), 0o600)
	})
}

// FsSaveScript writes src to %h/<name>.sieve atomically.
// Returns an error when name == ReservedScriptName.
func FsSaveScript(ctx context.Context, l locks.Locker, homeDir, name string, src []byte) error {
	if name == ReservedScriptName {
		return fmt.Errorf("sieve/scripts: %q is a reserved script name", name)
	}
	return withSieveLock(ctx, l, homeDir, func(ctx context.Context) error {
		target := namedScriptPath(homeDir, name)
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

// FsSetActive makes yarilo.sieve a symlink to %h/<name>.sieve.
// Returns an error when name == ReservedScriptName or the script does not exist.
func FsSetActive(ctx context.Context, l locks.Locker, homeDir, name string) error {
	if name == ReservedScriptName {
		return fmt.Errorf("sieve/scripts: %q is a reserved script name", name)
	}
	return withSieveLock(ctx, l, homeDir, func(ctx context.Context) error {
		link := activePath(homeDir)
		tmp := link + ".tmp"
		os.Remove(tmp) //nolint:errcheck
		if err := os.Symlink(name+sieveExt, tmp); err != nil {
			return fmt.Errorf("sieve/scripts: symlink: %w", err)
		}
		return os.Rename(tmp, link)
	})
}

// FsDeactivate removes yarilo.sieve so no named script is active.
// Next LMTP delivery will call FsInitUser which recreates the default.
func FsDeactivate(ctx context.Context, l locks.Locker, homeDir string) error {
	return withSieveLock(ctx, l, homeDir, func(ctx context.Context) error {
		err := os.Remove(activePath(homeDir))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	})
}

// FsDeleteScript removes %h/<name>.sieve.
// Returns an error when name == ReservedScriptName.
func FsDeleteScript(ctx context.Context, l locks.Locker, homeDir, name string) error {
	if name == ReservedScriptName {
		return fmt.Errorf("sieve/scripts: %q is a reserved script name", name)
	}
	return withSieveLock(ctx, l, homeDir, func(ctx context.Context) error {
		err := os.Remove(namedScriptPath(homeDir, name))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	})
}

// FsGetScript reads %h/<name>.sieve. Returns (nil, false, nil) when not found.
func FsGetScript(homeDir, name string) ([]byte, bool, error) {
	src, err := os.ReadFile(namedScriptPath(homeDir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("sieve/scripts: read %q: %w", name, err)
	}
	return src, true, nil
}

// FsListScripts returns names of all named scripts in homeDir.
// Excludes yarilo.sieve (the active-script entry point).
func FsListScripts(homeDir string) ([]string, error) {
	entries, err := os.ReadDir(homeDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sieve/scripts: readdir: %w", err)
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, sieveExt) {
			continue
		}
		if n == activeFile {
			continue // skip the active-script entry point
		}
		names = append(names, strings.TrimSuffix(n, sieveExt))
	}
	return names, nil
}

// FsRenameScript renames a script and updates the active symlink if needed.
// Both oldName and newName must not be ReservedScriptName.
func FsRenameScript(ctx context.Context, l locks.Locker, homeDir, oldName, newName string) error {
	if oldName == ReservedScriptName || newName == ReservedScriptName {
		return fmt.Errorf("sieve/scripts: %q is a reserved script name", ReservedScriptName)
	}
	return withSieveLock(ctx, l, homeDir, func(ctx context.Context) error {
		oldPath := namedScriptPath(homeDir, oldName)
		newPath := namedScriptPath(homeDir, newName)
		if err := os.Rename(oldPath, newPath); err != nil {
			return fmt.Errorf("sieve/scripts: rename: %w", err)
		}
		// Update symlink if oldName was active.
		active, err := FsActiveScriptName(homeDir)
		if err != nil || active != oldName {
			return err
		}
		link := activePath(homeDir)
		tmp := link + ".tmp"
		os.Remove(tmp) //nolint:errcheck
		if err := os.Symlink(newName+sieveExt, tmp); err != nil {
			return fmt.Errorf("sieve/scripts: symlink rename: %w", err)
		}
		return os.Rename(tmp, link)
	})
}

func lockOwner() string {
	return fmt.Sprintf("sieve:%d", os.Getpid())
}
