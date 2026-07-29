// Package logging installs the process-wide slog handler and keeps its level in
// a slog.LevelVar so verbosity can be changed at runtime (#889).
//
// Why a LevelVar rather than a fixed level: the level used to be read once at
// startup, so raising verbosity meant restarting the pod — which destroys the
// state being investigated. During the #878 analysis the interesting log window
// was lost twice for exactly that reason, once to a pod roll and once to
// cluster-wide debug rotating the container log away.
package logging

import (
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// level is the process-wide log level. slog.LevelVar is safe for concurrent use,
// so the handler reads it on every record without further synchronisation.
var level slog.LevelVar

var (
	// revertMu guards the pending auto-revert timer, so two overlapping
	// temporary raises cannot leave a stale timer that lowers the level while a
	// later request still wants it raised.
	revertMu    sync.Mutex
	revertTimer *time.Timer
	baseLevel   slog.Level
)

// Setup installs a JSON handler on stdout tagged with the service name and sets
// the initial level from the LOG_LEVEL environment variable. Unrecognised or
// empty values select info.
func Setup(service string) {
	lvl := ParseLevel(os.Getenv("LOG_LEVEL"))
	level.Set(lvl)
	baseLevel = lvl
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: &level,
	})).With("service", service))
}

// ParseLevel maps a level name to a slog.Level, defaulting to info for anything
// it does not recognise. Case-insensitive.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// LevelName renders a level the way ParseLevel accepts it.
func LevelName(l slog.Level) string {
	switch l {
	case slog.LevelDebug:
		return "debug"
	case slog.LevelWarn:
		return "warn"
	case slog.LevelError:
		return "error"
	case slog.LevelInfo:
		return "info"
	default:
		return strings.ToLower(l.String())
	}
}

// Level reports the level currently in effect.
func Level() slog.Level { return level.Level() }

// SetLevel changes the level for the remaining lifetime of the process and
// cancels any pending auto-revert.
func SetLevel(l slog.Level) {
	revertMu.Lock()
	if revertTimer != nil {
		revertTimer.Stop()
		revertTimer = nil
	}
	baseLevel = l
	revertMu.Unlock()
	level.Set(l)
}

// SetLevelFor raises (or lowers) the level for ttl, then restores what was in
// effect before.
//
// The TTL is the point of this function: "debug for the next 30 seconds while I
// reproduce" is a single call that cannot be left switched on by accident, which
// is how a debug toggle normally turns into a log that has rotated away by the
// time anyone reads it. ttl <= 0 is treated as a permanent SetLevel.
func SetLevelFor(l slog.Level, ttl time.Duration) {
	if ttl <= 0 {
		SetLevel(l)
		return
	}
	revertMu.Lock()
	if revertTimer != nil {
		revertTimer.Stop()
		revertTimer = nil
	} else {
		// First raise in this window: remember what to come back to.
		baseLevel = level.Level()
	}
	restore := baseLevel
	revertTimer = time.AfterFunc(ttl, func() {
		revertMu.Lock()
		revertTimer = nil
		revertMu.Unlock()
		level.Set(restore)
		slog.Info("logging: level reverted", "level", LevelName(restore))
	})
	revertMu.Unlock()
	level.Set(l)
}

// Var exposes the LevelVar for a handler built outside Setup (tests, tooling).
func Var() *slog.LevelVar { return &level }

// String renders the active level, so a caller can report it without importing
// slog.
func String() string { return LevelName(Level()) }
