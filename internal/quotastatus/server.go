// Package quotastatus implements the Postfix policy service protocol
// (RFC-style text key=value over TCP) for quota enforcement.
// Postfix connects via check_policy_service and asks whether a
// recipient's mailbox can accept the incoming message; the service
// returns action=DUNNO (allow) or action=REJECT 452 4.2.2 (full).
package quotastatus

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/dict"
	"github.com/0kaba0hub/yarilo/pkg/quota"
)

// Options configures the quota-status policy server.
type Options struct {
	// QuotaDict is the dict backend holding per-user quota counters.
	QuotaDict dict.Dict
	// Limits are the site-wide quota limits applied to every recipient.
	// Per-user limits require a userdb lookup (future phase).
	Limits quota.Limits
	// AliasDict resolves virtual aliases before quota lookup. The dict
	// key is the recipient address; the value is the destination address.
	// Nil disables alias resolution.
	AliasDict dict.Dict
	// AliasMaxHops is the maximum alias chain depth. Default: 5.
	AliasMaxHops int
}

// Server is the Postfix policy server.
type Server struct {
	opts Options
}

// New creates a Server.
func New(opts Options) *Server {
	return &Server{opts: opts}
}

// Serve accepts connections on ln until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("quotastatus: accept: %w", err)
		}
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Minute)) //nolint:errcheck
	sc := bufio.NewScanner(conn)
	bw := bufio.NewWriter(conn)

	for {
		attrs := make(map[string]string, 16)
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				break // blank line = end of this request
			}
			if eq := strings.IndexByte(line, '='); eq >= 0 {
				attrs[line[:eq]] = line[eq+1:]
			}
		}
		if sc.Err() != nil || len(attrs) == 0 {
			return
		}

		action := s.check(attrs)
		fmt.Fprintf(bw, "action=%s\n\n", action)
		if err := bw.Flush(); err != nil {
			return
		}
		conn.SetDeadline(time.Now().Add(5 * time.Minute)) //nolint:errcheck
	}
}

// check evaluates the Postfix policy request and returns the action string.
func (s *Server) check(attrs map[string]string) string {
	rawRecipient := strings.TrimSpace(attrs["recipient"])
	if rawRecipient == "" {
		return "DUNNO"
	}

	// Resolve through alias chain. Folder is always derived from the
	// original recipient's detail part (e.g. alice+Spam@ → folder=Spam)
	// so that per-folder ignore rules apply correctly.
	folder := extractFolder(rawRecipient)
	resolved := s.resolveAlias(context.Background(), rawRecipient)
	username := extractUsername(resolved)

	if s.opts.QuotaDict == nil {
		return "DUNNO"
	}

	effLim, ignore := s.opts.Limits.EffectiveLimits(folder)
	if ignore || (effLim.StorageBytes == 0 && effLim.Messages == 0) {
		return "DUNNO"
	}

	ctr := quota.NewCounter(s.opts.QuotaDict, username)
	u, err := ctr.Get(context.Background())
	if err != nil {
		slog.Warn("quotastatus: quota lookup failed", "user", username, "err", err)
		return "DUNNO" // fail-open
	}

	var msgSize int64
	if sz := strings.TrimSpace(attrs["size"]); sz != "" {
		msgSize, _ = strconv.ParseInt(sz, 10, 64)
	}

	if quota.IsOver(u, effLim, msgSize, 1) {
		slog.Info("quotastatus: reject over-quota",
			"user", username, "folder", folder,
			"storage_bytes", u.StorageBytes, "messages", u.Messages,
			"limit_bytes", effLim.StorageBytes, "msg_size", msgSize)
		return "REJECT 452 4.2.2 Mailbox full"
	}
	return "DUNNO"
}

// extractUsername strips the detail part from a recipient address:
// alice+tag@example.com → alice@example.com
func extractUsername(addr string) string {
	addr = strings.Trim(strings.TrimSpace(addr), "<>")
	at := strings.LastIndexByte(addr, '@')
	if at < 0 {
		return addr
	}
	local, domain := addr[:at], addr[at+1:]
	if plus := strings.IndexByte(local, '+'); plus >= 0 {
		local = local[:plus]
	}
	return local + "@" + domain
}

// extractFolder returns the delivery folder inferred from the recipient
// detail part: alice+Sent@example.com → "Sent", alice@example.com → "INBOX".
func extractFolder(addr string) string {
	addr = strings.Trim(strings.TrimSpace(addr), "<>")
	at := strings.LastIndexByte(addr, '@')
	local := addr
	if at >= 0 {
		local = addr[:at]
	}
	if plus := strings.IndexByte(local, '+'); plus >= 0 {
		if folder := local[plus+1:]; folder != "" {
			return folder
		}
	}
	return "INBOX"
}

// resolveAlias walks the alias chain for addr and returns the final
// mailbox address. Resolution order per hop:
//  1. Exact lookup of current address (e.g. "info@example.com")
//  2. If current has a detail part and exact lookup missed, retry
//     without the detail (e.g. "alice+tag@example.com" → "alice@example.com")
//
// Catch-all (@domain) and multiple-hop chains are handled by the
// configured SQL query — the server just iterates until stable.
// Returns addr unchanged when AliasDict is nil or the address is not found.
func (s *Server) resolveAlias(ctx context.Context, addr string) string {
	if s.opts.AliasDict == nil {
		return addr
	}
	maxHops := s.opts.AliasMaxHops
	if maxHops <= 0 {
		maxHops = 5
	}
	seen := make(map[string]struct{}, maxHops+1)
	current := addr
	for i := 0; i < maxHops; i++ {
		if _, dup := seen[current]; dup {
			break
		}
		seen[current] = struct{}{}

		if dest, ok := s.lookupAlias(ctx, current); ok {
			current = dest
			continue
		}
		// If current has a detail part, retry with the bare address.
		if bare := extractUsername(current); bare != current {
			if dest, ok := s.lookupAlias(ctx, bare); ok {
				current = dest
				continue
			}
		}
		break
	}
	return current
}

func (s *Server) lookupAlias(ctx context.Context, addr string) (string, bool) {
	vs, found, err := s.opts.AliasDict.Lookup(ctx, &dict.OpSettings{}, addr)
	if err != nil {
		slog.Debug("quotastatus: alias lookup error", "addr", addr, "err", err)
		return "", false
	}
	if !found || len(vs) == 0 {
		return "", false
	}
	dest := strings.TrimSpace(string(vs[0]))
	if dest == "" || dest == addr {
		return "", false
	}
	return dest, true
}
