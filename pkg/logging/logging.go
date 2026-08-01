// Package logging installs the process-wide slog handler with a runtime-changeable level.
package logging

import (
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

var level slog.LevelVar

var (
	// revertMu guards the pending auto-revert timer against overlapping temporary raises.
	revertMu    sync.Mutex
	revertTimer *time.Timer
	baseLevel   slog.Level
)

// Setup installs a JSON handler on stdout tagged with the service name.
// Initial level comes from LOG_LEVEL; unrecognised or empty values select info.
func Setup(service string) {
	lvl := ParseLevel(os.Getenv("LOG_LEVEL"))
	level.Set(lvl)
	baseLevel = lvl
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: &level,
	})).With("service", service))
}

// ParseLevel maps a level name (case-insensitive) to a slog.Level, defaulting to info.
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

// SetLevelFor changes the level for ttl, then restores the previous level.
// ttl <= 0 is treated as a permanent SetLevel.
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
		// first raise in this window: remember the level to restore
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

// String renders the active level name.
func String() string { return LevelName(Level()) }
