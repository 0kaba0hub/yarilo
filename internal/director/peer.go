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

	"github.com/0kaba0hub/yarilo/internal/cluster/ring"
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
		}
	}
	// lastDown==0: backend never went down → Up.
	// lastUp > lastDown: came up after last down → Up.
	// lastDown >= lastUp (incl. equal): went down at or after last up → Down.
	up := lastDown == 0 || lastUp > lastDown
	srv.ring.AddBackend(&ring.Backend{
		IP: ip, Port: port, Tag: tag, Up: up,
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
		if !srv.ring.SetUp(ip, true, ts) {
			// Backend not in our ring (arrived after handshake, no port info).
			// It will be picked up on the next reconnect handshake.
			slog.Warn("director: ring RING-CHANGE up for unknown backend", "ip", ip, "tag", tag)
		} else {
			slog.Info("director: ring change up", "ip", ip, "tag", tag)
		}
	case "down":
		srv.ring.RemoveBackend(ip)
		slog.Info("director: ring change down", "ip", ip)
	case "flush":
		srv.ring.SetUp(ip, false, ts)
		slog.Info("director: ring change flush", "ip", ip)
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
