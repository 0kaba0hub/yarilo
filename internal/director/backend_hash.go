package director

import (
	"fmt"
	"hash/crc32"
	"sort"
	"strings"

	"github.com/0kaba0hub/yarilo/internal/cluster/ring"
)

// backendSetHash computes a stable hash over the ROUTING-significant fields of
// the backend set (#846) — the yarilo analogue of Dovecot's mail_hosts_hash.
// Two directors that agree on routing produce the same hash; a dropped
// RING-CHANGE that leaves one director's backend set diverged shows up as a
// different hash, which ring status --all (#835) flags and PR-2 auto-heals.
//
// Only {ip, port, tag, vhosts, up} are hashed — exactly the fields that decide
// where a user routes. LastUp/LastDown (transient timestamps) and Hostname
// (cosmetic) are deliberately excluded so a cosmetic difference never reads as
// a routing divergence. Vhosts is normalized (0 == the default 100) so the two
// encodings of the same routing weight hash identically. The encoded records
// are sorted before hashing (a deterministic canonical order every director
// computes identically), so the hash is independent of map iteration order.
func backendSetHash(backends []ring.Backend) string {
	lines := make([]string, 0, len(backends))
	for _, b := range backends {
		vhosts := b.Vhosts
		if vhosts == 0 {
			vhosts = 100
		}
		lines = append(lines, fmt.Sprintf("%s:%d|%s|%d|%t", b.IP, b.Port, b.Tag, vhosts, b.Up))
	}
	sort.Strings(lines)
	return fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(strings.Join(lines, "\n"))))
}
