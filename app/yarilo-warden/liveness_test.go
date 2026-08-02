package main

import (
	"context"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/telemetry"
	"github.com/yarilomail/yarilo/internal/warden"
)

// TestWardenLivenessCheckHappy: a live server and an open (or absent) gate pass —
// the session mutex is acquirable.
func TestWardenLivenessCheckHappy(t *testing.T) {
	srv := warden.NewServer(10)
	if err := wardenLivenessCheck(srv, nil)(context.Background()); err != nil {
		t.Fatalf("a healthy server should pass: %v", err)
	}
	if err := wardenLivenessCheck(srv, telemetry.NewGate())(context.Background()); err != nil {
		t.Fatalf("open gate + healthy server should pass: %v", err)
	}
}

// TestWardenLivenessCheckNilServer: no server means the gate alone governs.
func TestWardenLivenessCheckNilServer(t *testing.T) {
	if err := wardenLivenessCheck(nil, nil)(context.Background()); err != nil {
		t.Fatalf("nil server should pass: %v", err)
	}
}

// TestWardenLivenessCheckGateWedged: a wedged gate fails on the deadline even
// though the server is fine — the injected-deadlock path.
func TestWardenLivenessCheckGateWedged(t *testing.T) {
	srv := warden.NewServer(10)
	gate := telemetry.NewGate()
	gate.Wedge()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := wardenLivenessCheck(srv, gate)(ctx); err == nil {
		t.Fatal("a wedged gate must fail the warden check")
	}
}
