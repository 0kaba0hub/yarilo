package main

import (
	"context"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	"github.com/0kaba0hub/yarilo/internal/telemetry"
)

// TestAuthLivenessCheckHappy: with a live cache and an open (or absent) gate the
// check passes — the cache mutex is acquirable.
func TestAuthLivenessCheckHappy(t *testing.T) {
	cache := protocol.NewCache(1<<20, time.Minute, time.Minute)
	if err := authLivenessCheck(cache, nil)(context.Background()); err != nil {
		t.Fatalf("a healthy cache should pass: %v", err)
	}
	if err := authLivenessCheck(cache, telemetry.NewGate())(context.Background()); err != nil {
		t.Fatalf("open gate + healthy cache should pass: %v", err)
	}
}

// TestAuthLivenessCheckNilCache: no cache configured means the gate alone
// governs — an open gate still passes.
func TestAuthLivenessCheckNilCache(t *testing.T) {
	if err := authLivenessCheck(nil, nil)(context.Background()); err != nil {
		t.Fatalf("nil cache should pass: %v", err)
	}
}

// TestAuthLivenessCheckGateWedged: a wedged gate fails the check on the context
// deadline even though the cache is fine — the injected-deadlock path.
func TestAuthLivenessCheckGateWedged(t *testing.T) {
	cache := protocol.NewCache(1<<20, time.Minute, time.Minute)
	gate := telemetry.NewGate()
	gate.Wedge()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := authLivenessCheck(cache, gate)(ctx); err == nil {
		t.Fatal("a wedged gate must fail the auth check")
	}
}
