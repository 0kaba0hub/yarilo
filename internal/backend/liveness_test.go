package backend

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/telemetry"
	"github.com/0kaba0hub/yarilo/pkg/config"
)

func TestStoreHealthPath(t *testing.T) {
	tests := []struct {
		name string
		sc   config.StorageConfig
		want string
	}{
		{"mail_path templated", config.StorageConfig{MailPath: "/mnt/mail/%d/%n"}, "/mnt/mail"},
		{"maildir_root fixed", config.StorageConfig{MaildirRoot: "/var/mail/"}, "/var/mail"},
		{"falls through to home template", config.StorageConfig{MailHomeTemplate: "/home/%u"}, "/home"},
		{"mail_path wins over root", config.StorageConfig{MailPath: "/a/%n", MaildirRoot: "/b"}, "/a"},
		{"template at root yields slash", config.StorageConfig{MailPath: "/%d"}, "/"},
		{"nothing configured", config.StorageConfig{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := storeHealthPath(tc.sc); got != tc.want {
				t.Fatalf("storeHealthPath = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStoreLivenessCheckStat: a reachable store passes, a missing one fails.
func TestStoreLivenessCheckStat(t *testing.T) {
	dir := t.TempDir()
	if err := storeLivenessCheck(dir, nil)(context.Background()); err != nil {
		t.Fatalf("existing store dir should pass: %v", err)
	}
	missing := filepath.Join(dir, "gone")
	if err := storeLivenessCheck(missing, nil)(context.Background()); err == nil {
		t.Fatal("a missing store path must fail the check")
	}
}

// TestStoreLivenessCheckGateWedged: a wedged gate fails the check on the context
// deadline even when the store path is fine — the injected-deadlock path.
func TestStoreLivenessCheckGateWedged(t *testing.T) {
	dir := t.TempDir()
	gate := telemetry.NewGate()
	gate.Wedge()
	check := storeLivenessCheck(dir, gate)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := check(ctx); err == nil {
		t.Fatal("a wedged gate must fail the check regardless of the store")
	}
}

// TestStoreLivenessCheckEmptyPathGateOnly: with no store path the gate leg alone
// governs — an open gate passes.
func TestStoreLivenessCheckEmptyPathGateOnly(t *testing.T) {
	if err := storeLivenessCheck("", telemetry.NewGate())(context.Background()); err != nil {
		t.Fatalf("empty path + open gate should pass: %v", err)
	}
}
