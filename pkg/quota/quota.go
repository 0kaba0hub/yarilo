// Package quota tracks and enforces per-user storage and message-count quota.
//
// Limits come from the userdb quota_rule= extra field (e.g. "*:storage=5G" or
// "Trash:storage=+1G"); a wildcard rule covers all mailboxes.
//
// Usage is stored as running counters in pkg/dict under two per-user keys,
// priv/quota/storage (bytes) and priv/quota/messages (count), applied
// atomically via dict.AtomicInc.
//
// IMAP QUOTA (RFC 9208) exposes GETQUOTAROOT and GETQUOTA; SETQUOTA is always
// rejected since limits are operator-set. LMTP checks quota before accepting a
// message and returns 452 "Mailbox full" when over.
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

	// RootName is the quota root name surfaced in IMAP QUOTA/QUOTAROOT responses.
	RootName = "User quota"
)

// FolderRule holds per-folder quota overrides parsed from a rule like
// "Trash:storage=+1G" or "Spam:ignore".
type FolderRule struct {
	StorageBytes     int64
	StorageAdditive  bool // true when value was prefixed with +
	Messages         int64
	MessagesAdditive bool
	Ignore           bool // messages in this folder don't count toward quota
}

// Limits carries the resolved per-user storage and message-count limits
// derived from the UserInfo.QuotaRules list. Zero means unlimited.
type Limits struct {
	StorageBytes int64 // 0 = unlimited
	Messages     int64 // 0 = unlimited
	PerFolder    map[string]FolderRule
}

// EffectiveLimits returns the limits that apply when operating on folder. The
// second value is true for an "ignore" folder, where the caller must skip both
// enforcement and counter updates. Additive rules (+N) add to the global
// limit; non-additive rules replace it. Exact folder name match only.
func (l Limits) EffectiveLimits(folder string) (Limits, bool) {
	rule, ok := l.PerFolder[folder]
	if !ok {
		return l, false
	}
	if rule.Ignore {
		return Limits{}, true
	}
	eff := Limits{StorageBytes: l.StorageBytes, Messages: l.Messages}
	if rule.StorageBytes > 0 {
		if rule.StorageAdditive {
			eff.StorageBytes = l.StorageBytes + rule.StorageBytes
		} else {
			eff.StorageBytes = rule.StorageBytes
		}
	}
	if rule.Messages > 0 {
		if rule.MessagesAdditive {
			eff.Messages = l.Messages + rule.Messages
		} else {
			eff.Messages = rule.Messages
		}
	}
	return eff, false
}

// ParseRules parses quota rule strings in the format [<mailbox>:]<resource>=<limit>
// and returns the aggregate Limits.
//
//	"*:storage=5G"        -> 5 GiB global storage limit
//	"*:messages=100000"   -> 100000 global message limit
//	"Trash:storage=+1G"   -> Trash gets global + 1 GiB
//	"Spam:ignore"         -> Spam messages don't count toward quota
func ParseRules(rules []string) Limits {
	var out Limits
	for _, r := range rules {
		parseRule(r, &out)
	}
	return out
}

func parseRule(rule string, out *Limits) {
	folder := "*"
	rest := rule
	if idx := strings.Index(rule, ":"); idx >= 0 {
		folder = strings.TrimSpace(rule[:idx])
		rest = rule[idx+1:]
	}

	// One rule may carry multiple key=value pairs separated by ":",
	// e.g. "*:bytes=1073741824:messages=100000".
	for _, spec := range strings.Split(rest, ":") {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}

		// "ignore": folder messages don't count toward quota.
		if strings.EqualFold(spec, "ignore") {
			if folder != "*" {
				if out.PerFolder == nil {
					out.PerFolder = make(map[string]FolderRule)
				}
				fr := out.PerFolder[folder]
				fr.Ignore = true
				out.PerFolder[folder] = fr
			}
			continue
		}

		eqIdx := strings.IndexByte(spec, '=')
		if eqIdx < 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(spec[:eqIdx]))
		val := strings.TrimSpace(spec[eqIdx+1:])
		additive := strings.HasPrefix(val, "+")

		if folder == "*" {
			switch key {
			case "storage", "bytes":
				out.StorageBytes = parseSize(val)
			case "messages", "message":
				out.Messages = parseCount(val)
			}
			continue
		}

		if out.PerFolder == nil {
			out.PerFolder = make(map[string]FolderRule)
		}
		fr := out.PerFolder[folder]
		switch key {
		case "storage", "bytes":
			fr.StorageBytes = parseSize(val)
			fr.StorageAdditive = additive
		case "messages", "message":
			fr.Messages = parseCount(val)
			fr.MessagesAdditive = additive
		}
		out.PerFolder[folder] = fr
	}
}

// ParseSize converts a size like "5G", "500M", "1T" or a plain byte count into
// bytes. "0" or empty means unlimited (0). Exported for config wiring.
func ParseSize(s string) int64 { return parseSize(s) }

// parseSize converts a size like "5G", "500M", "1T" or a plain byte count into
// bytes. "0" or empty means unlimited (0).
func parseSize(s string) int64 {
	if s == "" || s == "0" {
		return 0
	}
	// Leading + (additive) is treated as absolute here.
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

// Counter provides atomic per-user quota counter operations over a dict.Dict.
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

// Add increments the storage (bytes) and message counters by the given deltas;
// negative deltas decrement (on expunge). Uses read-compute-write so missing
// keys are initialised on first use, since dict.AtomicInc returns
// CommitNotFound for absent keys. Per-user sessions serialise via yarilo-locks,
// so the plain read-write is race-free in production.
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
	return IsOverWithGrace(u, limits, newBytes, newMsgs, 0)
}

// StorageBytesToKiB converts bytes to the RFC 9208 STORAGE unit
// (kibibytes, rounded up).
func StorageBytesToKiB(b int64) uint64 {
	if b <= 0 {
		return 0
	}
	return uint64((b + 1023) / 1024)
}
