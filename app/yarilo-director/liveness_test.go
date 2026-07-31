package main

import (
	"context"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/director"
	"github.com/0kaba0hub/yarilo/internal/telemetry"
)

// TestDirectorLivenessCheckHappy: a live server and an open (or absent) gate
// pass — the ring mutex is acquirable. (That Len actually blocks under a real
// write-hold is covered white-box in internal/cluster/ring.)
func TestDirectorLivenessCheckHappy(t *testing.T) {
	srv := director.NewWithOptions(director.Options{})
	if err := directorLivenessCheck(srv, nil)(context.Background()); err != nil {
		t.Fatalf("a healthy director should pass: %v", err)
	}
	if err := directorLivenessCheck(srv, telemetry.NewGate())(context.Background()); err != nil {
		t.Fatalf("open gate + healthy director should pass: %v", err)
	}
}

// TestDirectorLivenessCheckNilServer: no server means the gate alone governs.
func TestDirectorLivenessCheckNilServer(t *testing.T) {
	if err := directorLivenessCheck(nil, nil)(context.Background()); err != nil {
		t.Fatalf("nil server should pass: %v", err)
	}
}

// TestDirectorLivenessCheckGateWedged: a wedged gate fails on the deadline even
// though the ring is fine — the injected-deadlock path.
func TestDirectorLivenessCheckGateWedged(t *testing.T) {
	srv := director.NewWithOptions(director.Options{})
	gate := telemetry.NewGate()
	gate.Wedge()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := directorLivenessCheck(srv, gate)(ctx); err == nil {
		t.Fatal("a wedged gate must fail the director check")
	}
}
