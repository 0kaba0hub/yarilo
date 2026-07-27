package director

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// RingTopology is the cross-replica view (#833 PR-B): every reachable replica's
// own RingStatus plus a health verdict derived from comparing them. The whole
// point over a single-replica status is spotting DIVERGENCE — the failure mode
// that produced false "defects" during the #750 arc.
type RingTopology struct {
	SchemaVersion int           `json:"schemaVersion"`
	Healthy       bool          `json:"healthy"`
	Issues        []RingIssue   `json:"issues"`
	Replicas      []ReplicaView `json:"replicas"`
	// Assumptions records preconditions the matrix depends on, so a harness
	// knows what it is trusting (e.g. uniform api.listen across replicas).
	Assumptions []string `json:"assumptions"`
}

// ReplicaView is one replica's contribution: its own RingStatus when reachable,
// or the reason it could not be collected.
type ReplicaView struct {
	Addr      string      `json:"addr"`
	Reachable bool        `json:"reachable"`
	Error     string      `json:"error,omitempty"`
	Status    *RingStatus `json:"status,omitempty"`
}

// RingIssue is one detected problem. Only "error" severity flips Healthy to
// false; "warn" is informational (e.g. transient seq lag during activity) and
// deliberately does NOT fail the verdict — that separation is what keeps --all
// from crying wolf the way raw snapshots did.
type RingIssue struct {
	Severity string `json:"severity"` // "error" | "warn"
	Type     string `json:"type"`     // peer-unreachable | view-size-mismatch | backend-set-divergence | asymmetric-edge | tombstone-divergence | seq-lag
	Detail   string `json:"detail"`
}

// apiRingTopology aggregates every replica's ring view server-side and returns
// one authorized response (#833 PR-B). It fans out to each peer's OWN admin API
// (the single-replica GET /api/director/ring) with the shared per-release
// token, deriving each peer's API endpoint from its ring IP + THIS replica's
// api.listen port. That assumes every director runs the same api.listen (true
// for a Helm release sharing one ConfigMap) — recorded in Assumptions.
func (s *Server) apiRingTopology(w http.ResponseWriter, _ *http.Request) {
	self := s.membership.Status()

	apiPort := s.apiListenPort()
	views := map[string]RingStatus{self.Self: self}
	var unreachable []string
	errOf := map[string]string{}

	for _, m := range self.Members {
		if m.Self {
			continue
		}
		host, _, err := net.SplitHostPort(m.Addr)
		if err != nil {
			host = m.Addr
		}
		st, ferr := s.fetchPeerRing(net.JoinHostPort(host, strconv.Itoa(apiPort)))
		if ferr != nil {
			unreachable = append(unreachable, m.Addr)
			errOf[m.Addr] = ferr.Error()
			continue
		}
		views[m.Addr] = *st
	}

	topo := computeTopology(self.Self, views, unreachable, apiPort)
	for i := range topo.Replicas {
		if e, ok := errOf[topo.Replicas[i].Addr]; ok {
			topo.Replicas[i].Error = e
		}
	}
	apiJSON(w, topo)
}

// apiListenPort extracts the port from this replica's api.listen address.
func (s *Server) apiListenPort() int {
	_, portStr, err := net.SplitHostPort(s.apiAddr)
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(portStr)
	return p
}

const peerFetchTimeout = 3 * time.Second

// fetchPeerRing GETs a peer's single-replica ring status over plain HTTP (the
// admin API is not under internal_tls) with the shared Bearer token.
func (s *Server) fetchPeerRing(apiAddr string) (*RingStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), peerFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+apiAddr+"/api/director/ring", nil)
	if err != nil {
		return nil, fmt.Errorf("director/topology: build request: %w", err)
	}
	if s.apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.apiToken)
	}
	client := &http.Client{Timeout: peerFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("director/topology: get %s: %w", apiAddr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("director/topology: %s returned %d", apiAddr, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("director/topology: read %s: %w", apiAddr, err)
	}
	var st RingStatus
	if err := json.Unmarshal(body, &st); err != nil {
		return nil, fmt.Errorf("director/topology: decode %s: %w", apiAddr, err)
	}
	return &st, nil
}

// computeTopology is the pure verdict function (no I/O): given the collected
// per-replica views (keyed by ring addr, self included), the ring addrs that
// could not be collected, and the derived api port, it builds the matrix and
// the health verdict. Kept separate from the HTTP fan-out so the divergence
// logic is unit-testable with synthetic views.
func computeTopology(selfAddr string, views map[string]RingStatus, unreachable []string, apiPort int) RingTopology {
	var issues []RingIssue
	add := func(sev, typ, detail string) {
		issues = append(issues, RingIssue{Severity: sev, Type: typ, Detail: detail})
	}

	// (1) peer-unreachable — a member of this replica's view whose own view was
	// not collected. Counts as an error: never report healthy with a silently
	// missing peer, or --all shows green mid-partition.
	for _, addr := range unreachable {
		add("error", "peer-unreachable", fmt.Sprintf("%s is in membership but its view could not be collected", addr))
	}

	// Reference size = this replica's own view.
	refSize := 0
	if self, ok := views[selfAddr]; ok {
		refSize = self.Size
	}

	// Deterministic iteration over reachable replicas.
	reachAddrs := make([]string, 0, len(views))
	for a := range views {
		reachAddrs = append(reachAddrs, a)
	}
	sort.Strings(reachAddrs)

	// (2) view-size-mismatch.
	for _, a := range reachAddrs {
		if v := views[a]; v.Size != refSize {
			add("error", "view-size-mismatch", fmt.Sprintf("%s sees %d members; self (%s) sees %d", a, v.Size, selfAddr, refSize))
		}
	}

	// (2b) backend-set-divergence (#846) — reachable replicas that hash their
	// routing backend set differently have diverged (a dropped RING-CHANGE).
	// The reference is this replica's own hash; anything different is flagged.
	if self, ok := views[selfAddr]; ok && self.BackendSetHash != "" {
		for _, a := range reachAddrs {
			v := views[a]
			if v.BackendSetHash != "" && v.BackendSetHash != self.BackendSetHash {
				add("error", "backend-set-divergence", fmt.Sprintf("%s backend-set hash %s differs from self (%s) hash %s", a, v.BackendSetHash, selfAddr, self.BackendSetHash))
			}
		}
	}

	// (3) asymmetric-edge — A's self-row says right=B but B's self-row does not
	// say left=A (or the mirror on the left). Deduped by unordered pair.
	selfRowOf := func(v RingStatus) *RingMemberStatus {
		for i := range v.Members {
			if v.Members[i].Self {
				return &v.Members[i]
			}
		}
		return nil
	}
	seenPair := map[string]bool{}
	pairKey := func(x, y string) string {
		if x < y {
			return x + "|" + y
		}
		return y + "|" + x
	}
	for _, a := range reachAddrs {
		rowA := selfRowOf(views[a])
		if rowA == nil {
			continue
		}
		if rowA.Right != nil {
			b := *rowA.Right
			if vb, ok := views[b]; ok {
				rowB := selfRowOf(vb)
				if rowB != nil && (rowB.Left == nil || *rowB.Left != a) && !seenPair[pairKey(a, b)] {
					seenPair[pairKey(a, b)] = true
					add("error", "asymmetric-edge", fmt.Sprintf("%s.right=%s but %s does not see %s as its left", a, b, b, a))
				}
			}
		}
		if rowA.Left != nil {
			l := *rowA.Left
			if vl, ok := views[l]; ok {
				rowL := selfRowOf(vl)
				if rowL != nil && (rowL.Right == nil || *rowL.Right != a) && !seenPair[pairKey(a, l)] {
					seenPair[pairKey(a, l)] = true
					add("error", "asymmetric-edge", fmt.Sprintf("%s.left=%s but %s does not see %s as its right", a, l, l, a))
				}
			}
		}
	}

	// (4) tombstone-divergence — an addr live in some views but tombstoned in
	// others (propagation lag or a resurrected phantom).
	liveIn := map[string]bool{}
	deadIn := map[string]bool{}
	for _, a := range reachAddrs {
		v := views[a]
		for _, m := range v.Members {
			liveIn[m.Addr] = true
		}
		for _, t := range v.Tombstones {
			deadIn[t.Addr] = true
		}
	}
	divAddrs := make([]string, 0)
	for addr := range deadIn {
		if liveIn[addr] {
			divAddrs = append(divAddrs, addr)
		}
	}
	sort.Strings(divAddrs)
	for _, addr := range divAddrs {
		add("error", "tombstone-divergence", fmt.Sprintf("%s is a live member on some replicas but tombstoned on others", addr))
	}

	// (5) seq-lag — per origin, a reachable replica trailing the max watermark.
	// Warn only: watermarks legitimately differ during activity, and flipping
	// healthy on that is exactly the false defect we must avoid.
	maxSeq := map[string]uint64{}
	for _, a := range reachAddrs {
		for _, m := range views[a].Members {
			if m.Seq != nil && *m.Seq > maxSeq[m.Addr] {
				maxSeq[m.Addr] = *m.Seq
			}
		}
	}
	for _, a := range reachAddrs {
		seqOf := map[string]uint64{}
		for _, m := range views[a].Members {
			if m.Seq != nil {
				seqOf[m.Addr] = *m.Seq
			}
		}
		var trails []string
		origins := make([]string, 0, len(maxSeq))
		for o := range maxSeq {
			origins = append(origins, o)
		}
		sort.Strings(origins)
		for _, o := range origins {
			if maxSeq[o] > 0 && seqOf[o] < maxSeq[o] {
				trails = append(trails, fmt.Sprintf("%s@%d<%d", o, seqOf[o], maxSeq[o]))
			}
		}
		if len(trails) > 0 {
			add("warn", "seq-lag", fmt.Sprintf("%s trails on: %v", a, trails))
		}
	}

	// Build replica rows (self + reachable + unreachable), deterministic.
	rows := make([]ReplicaView, 0, len(views)+len(unreachable))
	for _, a := range reachAddrs {
		v := views[a]
		vc := v
		rows = append(rows, ReplicaView{Addr: a, Reachable: true, Status: &vc})
	}
	unreach := append([]string(nil), unreachable...)
	sort.Strings(unreach)
	for _, a := range unreach {
		rows = append(rows, ReplicaView{Addr: a, Reachable: false})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Addr < rows[j].Addr })

	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].Type != issues[j].Type {
			return issues[i].Type < issues[j].Type
		}
		return issues[i].Detail < issues[j].Detail
	})

	healthy := true
	for _, is := range issues {
		if is.Severity == "error" {
			healthy = false
			break
		}
	}

	return RingTopology{
		SchemaVersion: 1,
		Healthy:       healthy,
		Issues:        issues,
		Replicas:      rows,
		Assumptions: []string{
			fmt.Sprintf("peer API endpoints derived as <ring-ip>:%d — assumes uniform api.listen across replicas", apiPort),
			"admin API is plain HTTP guarded by Bearer token + api.allowed_nets; fan-out source is a director pod IP",
		},
	}
}
