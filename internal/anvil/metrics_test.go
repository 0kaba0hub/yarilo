package anvil

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestObserveRequestLabels(t *testing.T) {
	tests := []struct {
		name   string
		verb   string
		result string
	}{
		{"connect ok", "CONNECT", "ok"},
		{"connect refused", "CONNECT", "too_many_connections"},
		{"heartbeat ok", "HEARTBEAT", "ok"},
		{"heartbeat reaped", "HEARTBEAT", "session_unknown"},
		{"lookup", "LOOKUP", "ok"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := testutil.CollectAndCount(requestSeconds)
			observeRequest(tc.verb, tc.result, time.Now())
			if got := testutil.CollectAndCount(requestSeconds); got < before {
				t.Fatalf("series count shrank: %d → %d", before, got)
			}
		})
	}
}

func TestPenaltyLookupTaxonomy(t *testing.T) {
	tests := []struct {
		name   string
		result string
	}{
		{"no entry", "miss"},
		{"penalty in force", "hit"},
		{"decayed entry", "expired"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := testutil.ToFloat64(penaltyLookups.WithLabelValues(tc.result))
			penaltyLookups.WithLabelValues(tc.result).Inc()
			if got := testutil.ToFloat64(penaltyLookups.WithLabelValues(tc.result)); got != before+1 {
				t.Fatalf("%s counter = %v, want %v", tc.result, got, before+1)
			}
		})
	}
}

// TestKickMetricsEmittedAndDelivered proves the two counters that make a
// cross-replica kick assertable by scrape (#908 PR4): EMIT bumps kick_emitted,
// and a forwarded EVENT bumps kick_delivered. In production these land on
// different pods (EMIT on one, delivery on the pod holding the SUBSCRIBE); here
// one server exercises both sides.
func TestKickMetricsEmittedAndDelivered(t *testing.T) {
	_, addr := startTestServer(t, 0)

	sub, err := Dial(addr, nil, time.Second)
	if err != nil {
		t.Fatalf("dial sub: %v", err)
	}
	defer sub.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := sub.Subscribe(ctx, "kick:imap")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	beforeE := testutil.ToFloat64(kickEmitted)
	beforeD := testutil.ToFloat64(kickDelivered)

	pub, err := Dial(addr, nil, time.Second)
	if err != nil {
		t.Fatalf("dial pub: %v", err)
	}
	defer pub.Close()
	if err := pub.Emit("kick:imap", "sess-1"); err != nil {
		t.Fatalf("emit: %v", err)
	}

	select {
	case got := <-ch:
		if got != "sess-1" {
			t.Fatalf("payload = %q, want sess-1", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivered event")
	}

	if got := testutil.ToFloat64(kickEmitted); got != beforeE+1 {
		t.Fatalf("kick_emitted_total = %v, want %v", got, beforeE+1)
	}
	// Delivery is written in the subscriber's forward loop; give it a beat.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if testutil.ToFloat64(kickDelivered) == beforeD+1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := testutil.ToFloat64(kickDelivered); got != beforeD+1 {
		t.Fatalf("kick_delivered_total = %v, want %v", got, beforeD+1)
	}
}

// TestConnectTotalCounts asserts CONNECT outcomes land on connect_total by
// result, so the PR2 limit is checkable by number, not by scanning cnt:* keys.
func TestConnectTotalCounts(t *testing.T) {
	const max = 1
	_, addr := startTestServer(t, max)
	c := dialTestConn(t, addr)

	beforeOK := testutil.ToFloat64(connectTotal.WithLabelValues("ok"))
	beforeTM := testutil.ToFloat64(connectTotal.WithLabelValues("too_many_connections"))

	if got := c.cmd(t, "CONNECT\ts1\tu@d\t10.0.0.1\timap"); !strings.HasPrefix(got, "OK\t") {
		t.Fatalf("first CONNECT = %q, want OK", got)
	}
	if got := c.cmd(t, "CONNECT\ts2\tu@d\t10.0.0.1\timap"); !strings.Contains(got, "too-many-connections") {
		t.Fatalf("second CONNECT = %q, want refusal", got)
	}

	if got := testutil.ToFloat64(connectTotal.WithLabelValues("ok")); got != beforeOK+1 {
		t.Fatalf("connect_total{ok} = %v, want %v", got, beforeOK+1)
	}
	if got := testutil.ToFloat64(connectTotal.WithLabelValues("too_many_connections")); got != beforeTM+1 {
		t.Fatalf("connect_total{too_many_connections} = %v, want %v", got, beforeTM+1)
	}
}

// TestSweepReportsReapedSessions covers the signal that explains a
// reason=unknown HEARTBEAT: the sweeper dropped a session whose owner still
// believes it is alive.
func TestSweepReportsReapedSessions(t *testing.T) {
	srv := NewServer(10, WithSessionTTL(time.Nanosecond))
	mb := srv.state.(*memoryBackend)
	now := time.Now().UTC()
	mb.mu.Lock()
	mb.sessions["s1"] = &SessionInfo{ID: "s1", User: "u@d", IP: "10.0.0.1", lastSeen: now.Add(-time.Hour)}
	mb.sessions["s2"] = &SessionInfo{ID: "s2", User: "u@d", IP: "10.0.0.1", lastSeen: now.Add(-time.Hour)}
	mb.mu.Unlock()

	before := testutil.ToFloat64(sessionsReaped)
	mb.Maintain(now)
	if got := testutil.ToFloat64(sessionsReaped); got != before+2 {
		t.Fatalf("sessions_reaped_total = %v, want %v", got, before+2)
	}
	if got := testutil.ToFloat64(sessions); got != 0 {
		t.Fatalf("sessions gauge = %v, want 0 after sweep", got)
	}
}
