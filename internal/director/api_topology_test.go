package director

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/0kaba0hub/yarilo/internal/cluster/ring"
)

func sp(s string) *string { return &s }
func up(v uint64) *uint64 { return &v }

// mkView builds one replica's healthy view: members in the given (already
// ring-sorted) order, each with wrap-around left/right, and self marked. seqOf
// optionally sets per-origin watermarks.
func mkView(self string, order []string, seqOf map[string]uint64) RingStatus {
	n := len(order)
	members := make([]RingMemberStatus, 0, n)
	for i, a := range order {
		row := RingMemberStatus{Addr: a, Index: i, Self: a == self}
		if n >= 2 {
			l := order[(i-1+n)%n]
			r := order[(i+1)%n]
			row.Left, row.Right = &l, &r
		}
		if seqOf != nil {
			if v, ok := seqOf[a]; ok {
				row.Seq = up(v)
			}
		}
		members = append(members, row)
	}
	return RingStatus{SchemaVersion: 1, Self: self, Size: n, Members: members}
}

func hasIssue(issues []RingIssue, typ string) bool {
	for _, is := range issues {
		if is.Type == typ {
			return true
		}
	}
	return false
}

func TestComputeTopology_HealthyN3(t *testing.T) {
	order := []string{"10.0.0.1:9102", "10.0.0.2:9102", "10.0.0.3:9102"}
	views := map[string]RingStatus{}
	for _, a := range order {
		views[a] = mkView(a, order, nil)
	}

	topo := computeTopology(order[0], views, nil, 9103)
	if !topo.Healthy {
		t.Fatalf("a consistent N=3 ring must be healthy; issues=%+v", topo.Issues)
	}
	if len(topo.Issues) != 0 {
		t.Errorf("healthy ring must have no issues, got %+v", topo.Issues)
	}
	if len(topo.Replicas) != 3 {
		t.Errorf("want 3 replica rows, got %d", len(topo.Replicas))
	}
}

func TestComputeTopology_PeerUnreachable(t *testing.T) {
	order := []string{"10.0.0.1:9102", "10.0.0.2:9102", "10.0.0.3:9102"}
	// Only self + one peer collected; the third is unreachable.
	views := map[string]RingStatus{
		order[0]: mkView(order[0], order, nil),
		order[1]: mkView(order[1], order, nil),
	}
	topo := computeTopology(order[0], views, []string{order[2]}, 9103)
	if topo.Healthy {
		t.Fatal("an unreachable peer must make the verdict unhealthy (no silent green mid-partition)")
	}
	if !hasIssue(topo.Issues, "peer-unreachable") {
		t.Errorf("expected peer-unreachable issue, got %+v", topo.Issues)
	}
	// The unreachable replica must still appear as a row (reachable=false).
	var found bool
	for _, r := range topo.Replicas {
		if r.Addr == order[2] {
			found = true
			if r.Reachable {
				t.Errorf("unreachable replica row must have reachable=false")
			}
		}
	}
	if !found {
		t.Errorf("unreachable peer must still be listed as a replica row")
	}
}

func TestComputeTopology_ViewSizeMismatch(t *testing.T) {
	order := []string{"10.0.0.1:9102", "10.0.0.2:9102", "10.0.0.3:9102"}
	views := map[string]RingStatus{
		order[0]: mkView(order[0], order, nil),
		order[1]: mkView(order[1], order, nil),
		order[2]: mkView(order[2], order, nil),
	}
	// Replica 3 lost one member from its view.
	shrunk := views[order[2]]
	shrunk.Size = 2
	views[order[2]] = shrunk

	topo := computeTopology(order[0], views, nil, 9103)
	if topo.Healthy {
		t.Fatal("a view-size mismatch must be unhealthy")
	}
	if !hasIssue(topo.Issues, "view-size-mismatch") {
		t.Errorf("expected view-size-mismatch, got %+v", topo.Issues)
	}
}

func TestComputeTopology_AsymmetricEdge(t *testing.T) {
	order := []string{"10.0.0.1:9102", "10.0.0.2:9102", "10.0.0.3:9102"}
	a, b, c := order[0], order[1], order[2]
	views := map[string]RingStatus{
		a: mkView(a, order, nil),
		b: mkView(b, order, nil),
		c: mkView(c, order, nil),
	}
	// Break b's self-row left so it no longer points back at a (a.right=b but
	// b.left=c) — a half-open edge.
	vb := views[b]
	for i := range vb.Members {
		if vb.Members[i].Self {
			vb.Members[i].Left = sp(c)
		}
	}
	views[b] = vb

	topo := computeTopology(a, views, nil, 9103)
	if topo.Healthy {
		t.Fatal("an asymmetric ring edge must be unhealthy")
	}
	if !hasIssue(topo.Issues, "asymmetric-edge") {
		t.Errorf("expected asymmetric-edge, got %+v", topo.Issues)
	}
}

func TestComputeTopology_TombstoneDivergence(t *testing.T) {
	order := []string{"10.0.0.1:9102", "10.0.0.2:9102", "10.0.0.3:9102"}
	views := map[string]RingStatus{
		order[0]: mkView(order[0], order, nil),
		order[1]: mkView(order[1], order, nil),
		order[2]: mkView(order[2], order, nil),
	}
	// Replica 1 has tombstoned a member that replicas still list as live.
	v0 := views[order[0]]
	v0.Tombstones = []RingTombstoneInfo{{Addr: order[2], Age: "5s"}}
	views[order[0]] = v0

	topo := computeTopology(order[0], views, nil, 9103)
	if topo.Healthy {
		t.Fatal("a member live on some replicas but tombstoned on others must be unhealthy")
	}
	if !hasIssue(topo.Issues, "tombstone-divergence") {
		t.Errorf("expected tombstone-divergence, got %+v", topo.Issues)
	}
}

// TestComputeTopology_SeqLagIsWarnOnly locks in the false-defect guard: unequal
// seq watermarks (normal during activity) surface as a warn but must NOT flip
// the verdict to unhealthy.
func TestComputeTopology_SeqLagIsWarnOnly(t *testing.T) {
	order := []string{"10.0.0.1:9102", "10.0.0.2:9102", "10.0.0.3:9102"}
	views := map[string]RingStatus{
		order[0]: mkView(order[0], order, map[string]uint64{order[0]: 10}),
		order[1]: mkView(order[1], order, map[string]uint64{order[0]: 7}), // trails
		order[2]: mkView(order[2], order, map[string]uint64{order[0]: 10}),
	}
	topo := computeTopology(order[0], views, nil, 9103)
	if !topo.Healthy {
		t.Fatalf("seq lag alone must stay healthy, got issues=%+v", topo.Issues)
	}
	if !hasIssue(topo.Issues, "seq-lag") {
		t.Errorf("expected a seq-lag warn issue, got %+v", topo.Issues)
	}
	for _, is := range topo.Issues {
		if is.Type == "seq-lag" && is.Severity != "warn" {
			t.Errorf("seq-lag must be severity warn, got %q", is.Severity)
		}
	}
}

func TestApiListenPort(t *testing.T) {
	cases := []struct {
		addr string
		want int
	}{
		{":9103", 9103},
		{"0.0.0.0:9103", 9103},
		{"10.0.0.1:9103", 9103},
		{"bogus", 0},
	}
	for _, c := range cases {
		s := &Server{apiAddr: c.addr}
		if got := s.apiListenPort(); got != c.want {
			t.Errorf("apiListenPort(%q) = %d, want %d", c.addr, got, c.want)
		}
	}
}

// TestFetchPeerRing_SendsTokenAndDecodes validates the fan-out wiring: the
// shared Bearer token is sent, the single-replica path is hit, and a RingStatus
// body round-trips back.
func TestFetchPeerRing_SendsTokenAndDecodes(t *testing.T) {
	var gotAuth, gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		json.NewEncoder(w).Encode(RingStatus{SchemaVersion: 1, Self: "10.0.0.9:9102", Size: 1}) //nolint:errcheck
	}))
	defer ts.Close()

	s := &Server{apiToken: "sekret"}
	addr := ts.URL[len("http://"):] // httptest gives http://127.0.0.1:port
	st, err := s.fetchPeerRing(addr)
	if err != nil {
		t.Fatalf("fetchPeerRing: %v", err)
	}
	if gotAuth != "Bearer sekret" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer sekret")
	}
	if gotPath != "/api/director/ring" {
		t.Errorf("path = %q, want /api/director/ring", gotPath)
	}
	if st == nil || st.Self != "10.0.0.9:9102" || st.Size != 1 {
		t.Errorf("decoded status mismatch: %+v", st)
	}
}

func TestFetchPeerRing_Non200IsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer ts.Close()
	s := &Server{apiToken: "x"}
	if _, err := s.fetchPeerRing(ts.URL[len("http://"):]); err == nil {
		t.Fatal("a non-200 peer response must be an error, not a silent empty view")
	}
}

func TestComputeTopology_BackendSetDivergence(t *testing.T) {
	order := []string{"10.0.0.1:9102", "10.0.0.2:9102", "10.0.0.3:9102"}
	views := map[string]RingStatus{}
	for _, a := range order {
		v := mkView(a, order, nil)
		v.BackendSetHash = "aaaa1111" // all agree
		views[a] = v
	}

	// Healthy: identical hashes -> no divergence issue.
	topo := computeTopology(order[0], views, nil, 9103)
	if hasIssue(topo.Issues, "backend-set-divergence") {
		t.Fatalf("identical backend-set hashes must not flag divergence; issues=%+v", topo.Issues)
	}

	// One replica's backend set diverged (a dropped RING-CHANGE).
	diverged := views[order[2]]
	diverged.BackendSetHash = "bbbb2222"
	views[order[2]] = diverged

	topo = computeTopology(order[0], views, nil, 9103)
	if topo.Healthy {
		t.Fatal("a backend-set hash mismatch must be unhealthy")
	}
	if !hasIssue(topo.Issues, "backend-set-divergence") {
		t.Errorf("expected backend-set-divergence issue, got %+v", topo.Issues)
	}
}

// TestStatus_BackendSetHashReflectsRing verifies Status() hashes the live ring
// and that two directors with the same backends produce the same hash.
func TestStatus_BackendSetHashReflectsRing(t *testing.T) {
	sA, _ := startRingNode(t, "shared-secret", nil, 1)
	sB, _ := startRingNode(t, "shared-secret", nil, 1)
	for _, s := range []*Server{sA, sB} {
		s.ring.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 993, Tag: "imap", Up: true, Vhosts: 100})
		s.ring.AddBackend(&ring.Backend{IP: "10.0.0.2", Port: 993, Tag: "imap", Up: true, Vhosts: 100})
	}
	hA := sA.membership.Status().BackendSetHash
	hB := sB.membership.Status().BackendSetHash
	if hA == "" {
		t.Fatal("a populated ring must produce a non-empty backend-set hash")
	}
	if hA != hB {
		t.Errorf("same backend set must hash identically across replicas: %s vs %s", hA, hB)
	}

	// A divergent set on B must change its hash.
	sB.ring.RemoveBackend("10.0.0.2")
	if sB.membership.Status().BackendSetHash == hA {
		t.Error("removing a backend on one replica must change its backend-set hash")
	}
}
