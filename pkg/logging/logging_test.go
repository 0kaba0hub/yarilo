package logging

import (
	"log/slog"
	"testing"
	"time"
)

func TestParseLevel(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want slog.Level
	}{
		{"debug", "debug", slog.LevelDebug},
		{"mixed case", "DeBuG", slog.LevelDebug},
		{"padded", "  warn  ", slog.LevelWarn},
		{"warning alias", "warning", slog.LevelWarn},
		{"error", "error", slog.LevelError},
		{"info", "info", slog.LevelInfo},
		{"empty defaults to info", "", slog.LevelInfo},
		{"unknown defaults to info", "verbose", slog.LevelInfo},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseLevel(tc.in); got != tc.want {
				t.Fatalf("ParseLevel(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestLevelNameRoundTrip(t *testing.T) {
	for _, lvl := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
		name := LevelName(lvl)
		if got := ParseLevel(name); got != lvl {
			t.Fatalf("ParseLevel(LevelName(%v)) = %v, want %v", lvl, got, lvl)
		}
	}
}

func TestSetLevel(t *testing.T) {
	t.Cleanup(func() { SetLevel(slog.LevelInfo) })

	SetLevel(slog.LevelError)
	if got := Level(); got != slog.LevelError {
		t.Fatalf("Level() = %v, want error", got)
	}
	if got := String(); got != "error" {
		t.Fatalf("String() = %q, want \"error\"", got)
	}
}

// TestSetLevelForReverts covers the reason the TTL exists: a bounded raise must
// come back down on its own, so "debug for 30s while I reproduce" cannot be left
// switched on and rotate the log away.
func TestSetLevelForReverts(t *testing.T) {
	t.Cleanup(func() { SetLevel(slog.LevelInfo) })

	SetLevel(slog.LevelInfo)
	SetLevelFor(slog.LevelDebug, 60*time.Millisecond)
	if got := Level(); got != slog.LevelDebug {
		t.Fatalf("during TTL: Level() = %v, want debug", got)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if Level() == slog.LevelInfo {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("level did not revert, still %v", Level())
}

// TestSetLevelForOverlappingRaisesKeepBaseline guards the timer bookkeeping: a
// second raise while one is pending must not make the eventual revert land on
// the raised level instead of the original.
func TestSetLevelForOverlappingRaisesKeepBaseline(t *testing.T) {
	t.Cleanup(func() { SetLevel(slog.LevelInfo) })

	SetLevel(slog.LevelWarn)
	SetLevelFor(slog.LevelDebug, 40*time.Millisecond)
	SetLevelFor(slog.LevelDebug, 80*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if Level() == slog.LevelWarn {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected revert to warn, got %v", Level())
}

// TestSetLevelCancelsPendingRevert: an explicit permanent change must win over a
// timer that is still pending.
func TestSetLevelCancelsPendingRevert(t *testing.T) {
	t.Cleanup(func() { SetLevel(slog.LevelInfo) })

	SetLevel(slog.LevelInfo)
	SetLevelFor(slog.LevelDebug, 40*time.Millisecond)
	SetLevel(slog.LevelError)

	time.Sleep(120 * time.Millisecond)
	if got := Level(); got != slog.LevelError {
		t.Fatalf("Level() = %v, want error (pending revert should have been cancelled)", got)
	}
}

func TestSetLevelForZeroTTLIsPermanent(t *testing.T) {
	t.Cleanup(func() { SetLevel(slog.LevelInfo) })

	SetLevel(slog.LevelInfo)
	SetLevelFor(slog.LevelDebug, 0)
	time.Sleep(50 * time.Millisecond)
	if got := Level(); got != slog.LevelDebug {
		t.Fatalf("Level() = %v, want debug (ttl 0 means permanent)", got)
	}
}

func TestVarIsTheLevelUsedByLevel(t *testing.T) {
	t.Cleanup(func() { SetLevel(slog.LevelInfo) })

	Var().Set(slog.LevelWarn)
	if got := Level(); got != slog.LevelWarn {
		t.Fatalf("Level() = %v, want warn — Var must expose the same LevelVar", got)
	}
}
