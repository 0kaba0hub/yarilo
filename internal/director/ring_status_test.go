package director

import (
	"testing"
	"time"
)

// TestRingStatus_N1 verifies a singleton reports itself with no neighbors and
// no link — the smallest ring the JSON contract must render (left/right null).
func TestRingStatus_N1(t *testing.T) {
	srv, _ := startRingNode(t, "shared-secret", nil, 1)

	st := srv.membership.Status()
	if st.SchemaVersion != 1 {
		t.Fatalf("schemaVersion = %d, want 1", st.SchemaVersion)
	}
	if st.Size != 1 || len(st.Members) != 1 {
		t.Fatalf("N=1 must have exactly one member, got size=%d members=%d", st.Size, len(st.Members))
	}
	m := st.Members[0]
	if !m.Self {
		t.Errorf("the sole member must be marked self")
	}
	if m.Left != nil || m.Right != nil {
		t.Errorf("N=1 member must have nil left/right, got left=%v right=%v", m.Left, m.Right)
	}
	if m.Link != nil {
		t.Errorf("self must never carry a link, got %+v", m.Link)
	}
}

// TestRingStatus_N2_LinkBoth verifies that on a formed N=2 ring each replica
// sees the OTHER member as a single connected edge serving both directions
// ("both"), while its own row carries no link — and neighbors point across.
func TestRingStatus_N2_LinkBoth(t *testing.T) {
	srvA, addrA := startRingNode(t, "shared-secret", nil, 2)
	srvB, addrB := startRingNode(t, "shared-secret", []string{addrA}, 2)

	waitFor(t, 5*time.Second, func() bool {
		return len(srvA.membership.Members()) == 2 && len(srvB.membership.Members()) == 2
	})

	for _, tc := range []struct {
		name string
		srv  *Server
		self string
		peer string
	}{
		{"A", srvA, addrA, addrB},
		{"B", srvB, addrB, addrA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.srv.membership.Status()
			if st.Size != 2 {
				t.Fatalf("size = %d, want 2", st.Size)
			}
			var selfRow, peerRow *RingMemberStatus
			for i := range st.Members {
				switch st.Members[i].Addr {
				case tc.self:
					selfRow = &st.Members[i]
				case tc.peer:
					peerRow = &st.Members[i]
				}
			}
			if selfRow == nil || peerRow == nil {
				t.Fatalf("both rows must be present; got %+v", st.Members)
			}
			if !selfRow.Self || selfRow.Link != nil {
				t.Errorf("self row must be marked self with no link, got %+v", selfRow)
			}
			// At N=2 both neighbors are the same peer.
			if selfRow.Left == nil || selfRow.Right == nil || *selfRow.Left != tc.peer || *selfRow.Right != tc.peer {
				t.Errorf("N=2 self neighbors must both be the peer %s, got left=%v right=%v", tc.peer, selfRow.Left, selfRow.Right)
			}
			if peerRow.Link == nil {
				t.Fatalf("peer row must carry a link on a formed N=2 ring")
			}
			if peerRow.Link.Role != "both" {
				t.Errorf("N=2 link role = %q, want \"both\"", peerRow.Link.Role)
			}
			if peerRow.Link.State != "connected" || peerRow.Link.Since == nil {
				t.Errorf("N=2 link must be connected with a Since timestamp, got state=%q since=%v", peerRow.Link.State, peerRow.Link.Since)
			}
		})
	}
}
