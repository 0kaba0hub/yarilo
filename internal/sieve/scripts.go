package sieve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
)

const (
	sieveExt       = ".sieve"
	sieveLockTTL   = 5 * time.Second
	sieveLockRenew = 2 * time.Second

	// FallbackDefaultName is used when SieveConfig.DefaultName is not set.
	FallbackDefaultName = "yarilo"
)

// FsScriptStore manages per-user Sieve script files stored in the user's home
// directory. All files are hidden (dot-prefixed).
//
// Layout:
//
//	%h/.<DefaultName>.sieve   — active-script pointer (symlink → .<name>.sieve, or regular file with DefaultScriptBody)
//	%h/.<name>.sieve          — named user scripts
//	%h/.yarilo.sieve-vacation — vacation dedup state
type FsScriptStore struct {
	// DefaultName is the reserved script name (configured via sieve.default_name).
	// Cannot be used in PUTSCRIPT/DELETESCRIPT/RENAMESCRIPT/SETACTIVE.
	DefaultName string
	// Locker provides cross-process write coordination. Nil = single-process (tests).
	Locker locks.Locker
}

func (ss *FsScriptStore) DefaultScriptName() string { return ss.DefaultName }

func (ss *FsScriptStore) activeFile() string {
	return "." + ss.DefaultName + sieveExt
}

func (ss *FsScriptStore) activePath(homeDir string) string {
	return filepath.Join(homeDir, ss.activeFile())
}

func (ss *FsScriptStore) namedPath(homeDir, name string) string {
	return filepath.Join(homeDir, "."+name+sieveExt)
}

// withLock serialises this user's script-file operations (named scripts + the
// active-script pointer are interrelated, so they share one per-home key) —
// scoped to scripts only, independent of vacation / duplicate.
func (ss *FsScriptStore) withLock(ctx context.Context, username, homeDir string, fn func(context.Context) error) error {
	return withSieveLock(ctx, ss.Locker, username, "sieve-scripts:"+homeDir, fn)
}

// withSieveLock runs fn while holding the yarilo-locks lock named `resource`.
// When l is nil (tests, single-process) fn runs without locking. Each Sieve
// file family passes its own resource key so unrelated writes do not block one
// another.
func withSieveLock(ctx context.Context, l locks.Locker, username, resource string, fn func(context.Context) error) error {
	if l == nil {
		return fn(ctx)
	}
	// The id rides the context: the store interface has no place for one, and
	// managesieve and delivery both set it at their entry (#1672).
	return locks.WithLock(locks.WithSite(ctx, "sieve-write"), l, resource, locks.Owner(username, locks.IDFrom(ctx)),
		sieveLockTTL, sieveLockRenew, fn)
}

func (ss *FsScriptStore) ActiveScriptName(_ context.Context, _, homeDir string) (string, error) {
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

func (ss *FsScriptStore) LoadActiveScript(ctx context.Context, _, homeDir string) (src []byte, name string, err error) {
	src, err = os.ReadFile(ss.activePath(homeDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("sieve/scripts: read active: %w", err)
	}
	name, err = ss.ActiveScriptName(ctx, "", homeDir)
	return src, name, err
}

func (ss *FsScriptStore) InitUser(ctx context.Context, username, homeDir string) error {
	if _, err := os.Lstat(ss.activePath(homeDir)); err == nil {
		return nil
	}
	return ss.withLock(ctx, username, homeDir, func(ctx context.Context) error {
		if _, err := os.Lstat(ss.activePath(homeDir)); err == nil {
			return nil
		}
		if err := os.MkdirAll(homeDir, 0o700); err != nil {
			return fmt.Errorf("sieve/scripts: mkdir: %w", err)
		}
		return os.WriteFile(ss.activePath(homeDir), []byte(DefaultScriptBody), 0o600)
	})
}

func (ss *FsScriptStore) SaveScript(ctx context.Context, username, homeDir, name string, src []byte) error {
	if name == ss.DefaultName {
		return fmt.Errorf("sieve/scripts: %q is a reserved script name", name)
	}
	return ss.withLock(ctx, username, homeDir, func(ctx context.Context) error {
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

func (ss *FsScriptStore) SetActive(ctx context.Context, username, homeDir, name string) error {
	if name == ss.DefaultName {
		return fmt.Errorf("sieve/scripts: %q is a reserved script name", name)
	}
	return ss.withLock(ctx, username, homeDir, func(ctx context.Context) error {
		link := ss.activePath(homeDir)
		tmp := link + ".tmp"
		os.Remove(tmp) //nolint:errcheck
		if err := os.Symlink("."+name+sieveExt, tmp); err != nil {
			return fmt.Errorf("sieve/scripts: symlink: %w", err)
		}
		return os.Rename(tmp, link)
	})
}

func (ss *FsScriptStore) Deactivate(ctx context.Context, username, homeDir string) error {
	return ss.withLock(ctx, username, homeDir, func(ctx context.Context) error {
		err := os.Remove(ss.activePath(homeDir))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	})
}

func (ss *FsScriptStore) DeleteScript(ctx context.Context, username, homeDir, name string) error {
	if name == ss.DefaultName {
		return fmt.Errorf("sieve/scripts: %q is a reserved script name", name)
	}
	return ss.withLock(ctx, username, homeDir, func(ctx context.Context) error {
		err := os.Remove(ss.namedPath(homeDir, name))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	})
}

func (ss *FsScriptStore) GetScript(_ context.Context, _, homeDir, name string) ([]byte, bool, error) {
	src, err := os.ReadFile(ss.namedPath(homeDir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("sieve/scripts: read %q: %w", name, err)
	}
	return src, true, nil
}

func (ss *FsScriptStore) ListScripts(_ context.Context, _, homeDir string) ([]string, error) {
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

func (ss *FsScriptStore) RenameScript(ctx context.Context, username, homeDir, oldName, newName string) error {
	if oldName == ss.DefaultName || newName == ss.DefaultName {
		return fmt.Errorf("sieve/scripts: %q is a reserved script name", ss.DefaultName)
	}
	return ss.withLock(ctx, username, homeDir, func(ctx context.Context) error {
		if err := os.Rename(ss.namedPath(homeDir, oldName), ss.namedPath(homeDir, newName)); err != nil {
			return fmt.Errorf("sieve/scripts: rename: %w", err)
		}
		active, err := ss.ActiveScriptName(ctx, "", homeDir)
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

func (ss *FsScriptStore) VacationSent(ctx context.Context, _, homeDir, handle, senderAddr string) (bool, error) {
	return vacationSent(ctx, homeDir, handle, senderAddr)
}

func (ss *FsScriptStore) MarkVacationSent(ctx context.Context, username, homeDir, handle, senderAddr string, ttlSecs int) error {
	return markVacationSent(ctx, ss.Locker, username, homeDir, handle, senderAddr, ttlSecs)
}
