// Package director — shared helpers for applying state received from a
// ring/right-neighbor connection (membership.go owns the connection
// lifecycle itself; #750 phase 1 replaced the old full-mesh PeerDialer with
// the self-organizing ring).
package director

import (
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/internal/cluster/ring"
)

// applyHandshakeHost registers a backend received in the server's opening
// handshake. Wire format: HOST\t{ip}\t{port}\t{tag}\tD{down_ts}\tU{up_ts}\t{host}
// Up state is derived from timestamps: lastUp >= lastDown → Up, otherwise Down.
func applyHandshakeHost(srv *Server, line string) {
	fields := strings.Split(line, "\t")
	if len(fields) < 3 {
		return
	}
	ip := fields[1]
	port, err := strconv.Atoi(fields[2])
	if err != nil {
		return
	}
	tag := ""
	if len(fields) >= 4 {
		tag = fields[3]
	}
	var lastDown, lastUp int64
	var vhosts int
	for _, f := range fields[4:] {
		if len(f) < 2 {
			continue
		}
		v, pErr := strconv.ParseInt(f[1:], 10, 64)
		if pErr != nil {
			continue
		}
		switch f[0] {
		case 'D':
			lastDown = v
		case 'U':
			lastUp = v
		case 'V':
			vhosts = int(v) // ring weight (#706); absent on pre-#706 peers → 0 = default
		}
	}
	// lastDown==0: backend never went down → Up.
	// lastUp > lastDown: came up after last down → Up.
	// lastDown >= lastUp (incl. equal): went down at or after last up → Down.
	up := lastDown == 0 || lastUp > lastDown
	srv.ring.AddBackend(&ring.Backend{
		IP: ip, Port: port, Tag: tag, Up: up, Vhosts: vhosts,
		LastUp: lastUp, LastDown: lastDown,
	})
	slog.Debug("director: ring handshake backend", "ip", ip, "port", port, "tag", tag, "up", up)
}

// applyRingChangeFields processes a RING-CHANGE envelope payload: {ip}
// {event} [{tag}]. "up" re-enables a backend already known from handshake;
// "down" removes it; "flush" marks it unavailable for new lookups.
func applyRingChangeFields(srv *Server, payload []string) {
	if len(payload) < 2 {
		return
	}
	ip, event := payload[0], payload[1]
	tag := ""
	if len(payload) >= 3 {
		tag = payload[2]
	}
	ts := time.Now().Unix()
	switch event {
	case "up":
		srv.clearBackendTomb(ip) // #846: a real up event re-admits; drop any resync tombstone
		// A gossiped heartbeat carries the backend's seq (#776, field 3) and,
		// for lease-managed backends, its port + vhosts (fields 4-5). Refresh
		// the lease so a heartbeat that landed on ANY director keeps the
		// backend alive everywhere.
		var seq uint64
		if len(payload) >= 4 {
			if v, err := strconv.ParseUint(payload[3], 10, 64); err == nil {
				seq = v
				srv.recordBackendSeen(ip, seq)
			}
		}
		// When port is present, add the backend outright: the registration is
		// a persistent connection to exactly ONE of N directors (ClusterIP),
		// so the other directors only ever learn this backend via this gossip
		// — SetUp can't help them, it has no port. Full AddBackend closes the
		// "routed on 1 of 3 directors" gap.
		if len(payload) >= 6 {
			port, pErr := strconv.Atoi(payload[4])
			vhosts, _ := strconv.Atoi(payload[5])
			if pErr == nil {
				srv.ring.AddBackend(&ring.Backend{
					IP: ip, Port: port, Tag: tag, Up: true, Vhosts: vhosts, LastUp: ts,
				})
				slog.Info("director: ring change up", "ip", ip, "tag", tag, "port", port, "seq", seq)
				break
			}
		}
		if !srv.ring.SetUp(ip, true, ts) {
			// Backend not in our ring and no port carried (legacy admin/api
			// gossip): it will be picked up on the next reconnect handshake.
			slog.Warn("director: ring RING-CHANGE up for unknown backend", "ip", ip, "tag", tag)
		} else {
			slog.Info("director: ring change up", "ip", ip, "tag", tag)
		}
	case "vhosts":
		// Admin vhost-weight change (#706), replicated ring-wide. Deliberately
		// carries NO seq and never calls recordBackendSeen — an operator
		// reweighting a static backend must not turn it lease-managed (which
		// would make it expirable). Update the weight in place, preserving the
		// backend's up/down state.
		n := 0
		if len(payload) >= 4 {
			n, _ = strconv.Atoi(payload[3])
		}
		if b := srv.ring.GetBackend(ip); b != nil {
			b.Vhosts = n
			srv.ring.AddBackend(b)
			slog.Info("director: ring change vhosts", "ip", ip, "vhosts", n)
		} else {
			slog.Warn("director: ring RING-CHANGE vhosts for unknown backend", "ip", ip)
		}
	case "down":
		srv.recordBackendTomb(ip) // #846: block resync resurrection until a newer seq
		srv.forgetBackendLease(ip)
		srv.ring.RemoveBackend(ip)
		slog.Info("director: ring change down", "ip", ip)
	case "flush":
		srv.ring.SetUp(ip, false, ts)
		// Clear the flushed backend's pins on every replica (#706) so new
		// LOOKUPs rehash away from it. Sessions are untouched (drain) — the
		// origin decided whether to kick (admin evacuate) or not (wire drain).
		n := srv.userDir.DeleteByBackend(ip)
		slog.Info("director: ring change flush", "ip", ip, "pins_cleared", n)
	}
}

// applyUserMovedFields processes a USER-MOVED envelope payload: {user} {ip} {port}.
func applyUserMovedFields(srv *Server, payload []string) {
	if len(payload) < 3 {
		return
	}
	user, ip, portStr := payload[0], payload[1], payload[2]
	addr := net.JoinHostPort(ip, portStr)
	srv.userDir.Set(user, addr, false)
	slog.Debug("director: ring user-moved", "user", user, "backend", addr)
}
