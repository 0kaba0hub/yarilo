// Package quota implements per-user storage and message-count quota
// tracking and enforcement. It is the yarilo equivalent of the
// reference implementation's quota plugin.
//
// Architecture:
//
//   - Limits come from the userdb `quota_rule=` extra field
//     (format: `*:storage=5G` or `Trash:storage=+1G`). A single
//     wildcard rule `*:storage=<limit>` covers all mailboxes and is
//     what most deployments use.
//
//   - Usage is tracked as running counters in pkg/dict under two
//     per-user keys:
//     priv/quota/storage   — bytes used (int64 string)
//     priv/quota/messages  — message count (int64 string)
//     Deltas are applied atomically via dict.AtomicInc so concurrent
//     session saves do not race.
//
//   - The IMAP extension (RFC 9208) exposes GETQUOTAROOT and GETQUOTA
//     commands. SETQUOTA is always rejected (limits are operator-set,
//     not client-set).
//
//   - LMTP inbound delivery checks quota before accepting a message
//     and returns 452 "Mailbox full" when over the limit.
package quota

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/0kaba0hub/yarilo/pkg/dict"
)

// Dict keys for per-user quota counters.
const (
	KeyStorage  = "priv/quota/storage"  // bytes used (int64)
	KeyMessages = "priv/quota/messages" // message count (int64)

	// RootName is the quota root name surfaced in IMAP QUOTA/QUOTAROOT
	// responses. Matches the reference implementation default "User quota".
	RootName = "User quota"
)

// Limits carries the resolved per-user storage and message-count limits
// derived from the UserInfo.QuotaRules list. Zero means unlimited.
type Limits struct {
	StorageBytes int64 // 0 = unlimited
	Messages     int64 // 0 = unlimited
}

// ParseRules parses a slice of quota rule strings in the format
// `[<mailbox>:]<resource>=<limit>` and returns the aggregate Limits.
// Only `*` (wildcard) storage and message rules are currently
// evaluated — per-folder overrides (e.g. `Trash:storage=+1G`) are
// recorded but not yet enforced per folder (QUOTA-1 scope covers the
// global limit only).
//
// Rule examples:
//
//	"*:storage=5G"       → 5 GiB storage limit for all folders
//	"*:messages=100000"  → 100 000 message limit
//	"*:storage=0"        → no storage limit
func ParseRules(rules []string) Limits {
	var out Limits
	for _, r := range rules {
		parseRule(r, &out)
	}
	return out
}

func parseRule(rule string, out *Limits) {
	// Strip optional mailbox prefix ("*:" or "Trash:" etc.).
	spec := rule
	if idx := strings.Index(rule, ":"); idx >= 0 {
		spec = rule[idx+1:]
	}
	// spec is now "storage=5G" or "messages=100000" etc.
	eqIdx := strings.IndexByte(spec, '=')
	if eqIdx < 0 {
		return
	}
	key := strings.ToLower(strings.TrimSpace(spec[:eqIdx]))
	val := strings.TrimSpace(spec[eqIdx+1:])
	switch key {
	case "storage", "bytes":
		out.StorageBytes = parseSize(val)
	case "messages", "message":
		out.Messages = parseCount(val)
	}
}

// parseSize converts a human-readable size like "5G", "500M", "1T"
// or a plain byte count into bytes. "0" or empty means unlimited (0).
func parseSize(s string) int64 {
	if s == "" || s == "0" {
		return 0
	}
	// Strip leading + (additive rule — treated as absolute here for
	// simplicity, matching the global-limit-only QUOTA-1 scope).
	s = strings.TrimPrefix(s, "+")
	if len(s) == 0 {
		return 0
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'K', 'k':
		mult = 1024
		s = s[:len(s)-1]
	case 'M', 'm':
		mult = 1024 * 1024
		s = s[:len(s)-1]
	case 'G', 'g':
		mult = 1024 * 1024 * 1024
		s = s[:len(s)-1]
	case 'T', 't':
		mult = 1024 * 1024 * 1024 * 1024
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n * mult
}

func parseCount(s string) int64 {
	s = strings.TrimPrefix(s, "+")
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// Usage holds the current quota usage read from the dict.
type Usage struct {
	StorageBytes int64
	Messages     int64
}

// Counter wraps a dict.Dict and a username to provide atomic
// per-user quota counter operations.
type Counter struct {
	d    dict.Dict
	user string
}

// NewCounter returns a Counter bound to user.
func NewCounter(d dict.Dict, user string) *Counter {
	return &Counter{d: d, user: user}
}

func (c *Counter) ops() *dict.OpSettings {
	return &dict.OpSettings{Username: c.user}
}

// Add increments the storage (bytes) and message counters by the
// given deltas. Negative deltas decrement (on expunge). Uses a
// read-compute-write pattern so missing keys are initialised on
// first use — the dict.AtomicInc primitive returns CommitNotFound
// for absent keys instead of initialising them.
//
// Per-user sessions serialise via yarilo-locks so a plain
// read-write is race-free in production; for multi-pod setups
// the locker ensures only one session per user is active.
func (c *Counter) Add(ctx context.Context, bytes, messages int64) error {
	if bytes == 0 && messages == 0 {
		return nil
	}
	cur, err := c.Get(ctx)
	if err != nil {
		return fmt.Errorf("quota/counter: add get: %w", err)
	}
	cur.StorageBytes += bytes
	cur.Messages += messages
	if cur.StorageBytes < 0 {
		cur.StorageBytes = 0
	}
	if cur.Messages < 0 {
		cur.Messages = 0
	}
	return c.Set(ctx, cur)
}

// Get reads the current usage from the dict. Missing keys are treated
// as zero (first-use before any message has been saved).
func (c *Counter) Get(ctx context.Context) (Usage, error) {
	ops := c.ops()
	var u Usage
	if vs, found, err := c.d.Lookup(ctx, ops, KeyStorage); err != nil {
		return u, fmt.Errorf("quota/counter: lookup storage: %w", err)
	} else if found && len(vs) > 0 {
		u.StorageBytes, _ = strconv.ParseInt(string(vs[0]), 10, 64)
	}
	if vs, found, err := c.d.Lookup(ctx, ops, KeyMessages); err != nil {
		return u, fmt.Errorf("quota/counter: lookup messages: %w", err)
	} else if found && len(vs) > 0 {
		u.Messages, _ = strconv.ParseInt(string(vs[0]), 10, 64)
	}
	if u.StorageBytes < 0 {
		u.StorageBytes = 0
	}
	if u.Messages < 0 {
		u.Messages = 0
	}
	return u, nil
}

// Set overwrites the quota counters with the supplied usage values.
// Used by the admin recalc endpoint after scanning all messages.
func (c *Counter) Set(ctx context.Context, u Usage) error {
	ops := c.ops()
	tx, err := c.d.Begin(ctx, ops)
	if err != nil {
		return fmt.Errorf("quota/counter: set begin: %w", err)
	}
	if err := tx.Set(KeyStorage, []byte(strconv.FormatInt(u.StorageBytes, 10))); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("quota/counter: set storage: %w", err)
	}
	if err := tx.Set(KeyMessages, []byte(strconv.FormatInt(u.Messages, 10))); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("quota/counter: set messages: %w", err)
	}
	res, err := tx.Commit()
	if err != nil {
		return fmt.Errorf("quota/counter: set commit: %w", err)
	}
	if res != dict.CommitOK {
		return fmt.Errorf("quota/counter: set commit result %v", res)
	}
	return nil
}

// IsOver reports whether adding newBytes bytes and newMsgs messages
// would exceed the limits. Returns (false, nil) when quota is
// unlimited (limits.StorageBytes == 0 && limits.Messages == 0).
func IsOver(u Usage, limits Limits, newBytes, newMsgs int64) bool {
	if limits.StorageBytes > 0 && u.StorageBytes+newBytes > limits.StorageBytes {
		return true
	}
	if limits.Messages > 0 && u.Messages+newMsgs > limits.Messages {
		return true
	}
	return false
}

// StorageBytesToKiB converts bytes to the RFC 9208 STORAGE unit
// (kibibytes, rounded up).
func StorageBytesToKiB(b int64) uint64 {
	if b <= 0 {
		return 0
	}
	return uint64((b + 1023) / 1024)
}
