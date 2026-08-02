package director

import (
	"fmt"
	"net"
	"sort"
	"strconv"

	"github.com/yarilomail/yarilo/internal/cluster/ring"
)

// Assignment policies for the INITIAL (unpinned) user→backend placement (#797).
// Sticky pins and USER-MOVE are unaffected — only a fresh
// assignment consults the policy.
const (
	policyHash          = "hash"           // consistent hash (default; reference semantics)
	policyLeastSessions = "least_sessions" // load-aware, two-level normalized
)

func (s *Server) assignmentPolicy() string {
	if s.opts.AssignmentPolicy == policyLeastSessions {
		return policyLeastSessions
	}
	return policyHash
}

// pickBackend chooses the backend for an UNPINNED user within the requested tag.
//
//   - hash (default): the consistent-hash backend for the tag.
//   - least_sessions: the least-loaded Up backend in the tag by a two-level,
//     capacity-normalized load. Level 1 = the requested protocol's sessions;
//     level 2 = total sessions, deciding among level-1 ties. Each load is
//     normalized as count*100/vhosts (vhosts 1..100; 0 = drain → excluded).
//     Tie-break: lower (IP, port). When reqProto is "" (admin path — no protocol)
//     level 1 is skipped and total load decides.
//
// Strict tag match: candidates are only Up backends whose Tag == tag ("" is a
// real tag, the untagged pool) with vhosts > 0. No full-ring fallback — an empty
// candidate set returns nil and the caller FAILs.
func (s *Server) pickBackend(user, tag, reqProto string) *ring.Backend {
	if s.assignmentPolicy() != policyLeastSessions {
		return s.ring.LookupBackendByTag(user, tag)
	}

	total, byProto := s.sessionCounts()
	type cand struct {
		b      ring.Backend
		l1, l2 int
	}
	var cands []cand
	for _, b := range s.ring.Backends() {
		if !b.Up || b.Tag != tag || b.Vhosts <= 0 {
			continue // strict tag; drain (vhosts 0) and down backends excluded
		}
		pc := 0
		if m := byProto[b.IP]; m != nil {
			pc = m[reqProto]
		}
		cands = append(cands, cand{
			b:  b,
			l1: pc * 100 / b.Vhosts,
			l2: total[b.IP] * 100 / b.Vhosts,
		})
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool {
		ci, cj := cands[i], cands[j]
		if reqProto != "" && ci.l1 != cj.l1 {
			return ci.l1 < cj.l1
		}
		if ci.l2 != cj.l2 {
			return ci.l2 < cj.l2
		}
		if ci.b.IP != cj.b.IP {
			return ci.b.IP < cj.b.IP
		}
		return ci.b.Port < cj.b.Port
	})
	best := cands[0].b
	return &best
}

// assignAndPin resolves an UNPINNED user via the policy, records the pin, and
// propagates USER-ASSIGN — the SINGLE owner of initial placement (#797). Every
// fresh-assignment caller (login LOOKUP, LMTP RouteUser, admin apiMap under
// least_sessions) funnels through here so none can independently pick a
// different pod and split a user's per-user writer (#788). Returns nil when no
// backend is available.
func (s *Server) assignAndPin(user, tag, reqProto string) *ring.Backend {
	b := s.pickBackend(user, tag, reqProto)
	if b == nil {
		return nil
	}
	addr := net.JoinHostPort(b.IP, strconv.Itoa(b.Port))
	h := s.userDir.Set(user, addr, false)
	if seq, by, ok := s.userDir.LastAssign(h); ok {
		s.membership.originate("USER-ASSIGN", fmt.Sprintf("%d\t%s\t%d\t%s", h, addr, seq, by))
	}
	return b
}
