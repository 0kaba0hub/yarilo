package director

import (
	"sort"
	"testing"

	"github.com/0kaba0hub/yarilo/internal/cluster/ring"
)

func backendIPs(s *Server, tag string) []string {
	var out []string
	for _, b := range s.ring.Backends() {
		if b.Tag == tag {
			out = append(out, b.IP)
		}
	}
	sort.Strings(out)
	return out
}

// TestReconcileDNSBackends covers #776: DNS is authoritative for backend
// existence — new IPs added, vanished IPs (rescheduled backend, or a stale
// gossiped/handshake entry) pruned — while admin-added backends and a
// failed/empty resolution are left alone.
func TestReconcileDNSBackends(t *testing.T) {
	srv := NewWithOptions(Options{})

	// Initial resolution: two live pods.
	srv.ReconcileDNSBackends("imap", 10143, []string{"10.0.0.1", "10.0.0.2"})
	if got := backendIPs(srv, "imap"); len(got) != 2 {
		t.Fatalf("after initial resolve want 2 backends, got %v", got)
	}

	// A stale entry a peer gossiped (Source "") for the same tag — must be
	// pruned by the next reconcile since it is not in DNS.
	srv.ring.AddBackend(&ring.Backend{IP: "10.0.0.99", Port: 10143, Tag: "imap", Up: true})
	// An admin-added backend — must SURVIVE reconciliation.
	srv.ring.AddBackend(&ring.Backend{IP: "10.9.9.9", Port: 10143, Tag: "imap", Up: true, Source: "admin"})

	// Rollout: pod .2 rescheduled to .3; DNS now returns .1 and .3.
	srv.ReconcileDNSBackends("imap", 10143, []string{"10.0.0.1", "10.0.0.3"})

	got := backendIPs(srv, "imap")
	want := []string{"10.0.0.1", "10.0.0.3", "10.9.9.9"} // .2 gone, .99 pruned, admin kept, .3 added
	if len(got) != len(want) {
		t.Fatalf("after rollout reconcile got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("after rollout reconcile got %v, want %v", got, want)
		}
	}

	// Empty/failed resolution must NOT prune anything (no blackhole).
	srv.ReconcileDNSBackends("imap", 10143, nil)
	if got := backendIPs(srv, "imap"); len(got) != 3 {
		t.Fatalf("empty resolution must not prune, got %v", got)
	}
}
