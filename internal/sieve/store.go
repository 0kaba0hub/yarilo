package sieve

import (
	"context"

	"github.com/0kaba0hub/yarilo/pkg/dict"
	"github.com/0kaba0hub/yarilo/pkg/locks"
)

// ScriptStore is the storage contract for per-user Sieve scripts and
// vacation dedup state. Two implementations exist: FsScriptStore (files
// in the user's home directory) and DictScriptStore (dict/redis backend).
type ScriptStore interface {
	// InitUser seeds the active-script pointer on first delivery.
	InitUser(ctx context.Context, username, homeDir string) error

	// LoadActiveScript returns the source and name of the currently active
	// script. Returns (nil, "", nil) when no active script exists.
	LoadActiveScript(ctx context.Context, username, homeDir string) (src []byte, name string, err error)

	// ActiveScriptName returns the name of the active script, or "" if none.
	ActiveScriptName(ctx context.Context, username, homeDir string) (string, error)

	// SaveScript writes src as a named script.
	SaveScript(ctx context.Context, username, homeDir, name string, src []byte) error

	// GetScript reads a named script. Returns (nil, false, nil) when not found.
	GetScript(ctx context.Context, username, homeDir, name string) ([]byte, bool, error)

	// ListScripts returns names of all stored named scripts.
	ListScripts(ctx context.Context, username, homeDir string) ([]string, error)

	// SetActive makes name the active script.
	SetActive(ctx context.Context, username, homeDir, name string) error

	// Deactivate removes the active pointer.
	Deactivate(ctx context.Context, username, homeDir string) error

	// DeleteScript removes a named script.
	DeleteScript(ctx context.Context, username, homeDir, name string) error

	// RenameScript renames a script and updates the active pointer if needed.
	RenameScript(ctx context.Context, username, homeDir, oldName, newName string) error

	// VacationSent reports whether a vacation reply was already sent for
	// handle+senderAddr and has not yet expired.
	VacationSent(ctx context.Context, username, homeDir, handle, senderAddr string) (bool, error)

	// MarkVacationSent records a sent vacation reply with a TTL of ttlSecs.
	MarkVacationSent(ctx context.Context, username, homeDir, handle, senderAddr string, ttlSecs int) error

	// DefaultScriptName returns the reserved script name that cannot be used
	// in PUTSCRIPT/DELETESCRIPT/RENAMESCRIPT/SETACTIVE.
	DefaultScriptName() string
}

// NewScriptStore returns an FsScriptStore when driver is "fs" (or empty),
// and a DictScriptStore for any other value. The dict parameter is ignored
// for the fs driver.
func NewScriptStore(driver, defaultName string, locker locks.Locker, d dict.Dict) ScriptStore {
	if defaultName == "" {
		defaultName = FallbackDefaultName
	}
	if driver == "" || driver == "fs" {
		return &FsScriptStore{DefaultName: defaultName, Locker: locker}
	}
	return &DictScriptStore{defaultName: defaultName, locker: locker, d: d}
}
