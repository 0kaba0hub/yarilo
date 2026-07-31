package main

import (
	"context"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/anvil"
	"github.com/0kaba0hub/yarilo/internal/telemetry"
)

// TestAnvilLivenessCheckHappy: a live server and an open (or absent) gate pass —
// the session mutex is acquirable.
func TestAnvilLivenessCheckHappy(t *testing.T) {
	srv := anvil.NewServer(10)
	if err := anvilLivenessCheck(srv, nil)(context.Background()); err != nil {
		t.Fatalf("a healthy server should pass: %v", err)
	}
	if err := anvilLivenessCheck(srv, telemetry.NewGate())(context.Background()); err != nil {
		t.Fatalf("open gate + healthy server should pass: %v", err)
	}
}

// TestAnvilLivenessCheckNilServer: no server means the gate alone governs.
func TestAnvilLivenessCheckNilServer(t *testing.T) {
	if err := anvilLivenessCheck(nil, nil)(context.Background()); err != nil {
		t.Fatalf("nil server should pass: %v", err)
	}
}

// TestAnvilLivenessCheckGateWedged: a wedged gate fails on the deadline even
// though the server is fine — the injected-deadlock path.
func TestAnvilLivenessCheckGateWedged(t *testing.T) {
	srv := anvil.NewServer(10)
	gate := telemetry.NewGate()
	gate.Wedge()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := anvilLivenessCheck(srv, gate)(ctx); err == nil {
		t.Fatal("a wedged gate must fail the anvil check")
	}
}
