package quota

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"

	"github.com/0kaba0hub/yarilo/pkg/dict"
)

// Clone mirrors the authoritative count usage into one or more external dicts
// (the quota_clone mirror). It is NEVER the source of truth — enforcement always
// reads the index. The mirror exists only for external consumers (dashboards,
// tooling, a policy service that cannot open the mailbox). yarilo fans out to
// SEVERAL targets at once (e.g. SQL + Redis), which the single-dict reference
// cannot do.
type Clone struct {
	targets []dict.Dict
}

// NewClone returns a Clone over targets, or nil when none are configured so
// callers can hold a *Clone unconditionally and treat nil as "disabled".
func NewClone(targets []dict.Dict) *Clone {
	if len(targets) == 0 {
		return nil
	}
	return &Clone{targets: targets}
}

// Write mirrors u for user into every target concurrently. Each target is
// best-effort: a failing one is logged and never blocks the others or the
// caller's authoritative path. A nil *Clone is a no-op. Uses the reference-
// compatible keys priv/quota/storage and priv/quota/messages so an operator can
// point external readers at the same layout.
func (c *Clone) Write(ctx context.Context, user string, u Usage) {
	if c == nil {
		return
	}
	var wg sync.WaitGroup
	for _, d := range c.targets {
		wg.Add(1)
		go func(d dict.Dict) {
			defer wg.Done()
			if err := writeClone(ctx, d, user, u); err != nil {
				slog.Warn("quota clone write failed", "dict", d.Name(), "user", user, "err", err)
			}
		}(d)
	}
	wg.Wait()
}

func writeClone(ctx context.Context, d dict.Dict, user string, u Usage) error {
	tx, err := d.Begin(ctx, &dict.OpSettings{Username: user})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if err := tx.Set(KeyStorage, []byte(strconv.FormatInt(u.StorageBytes, 10))); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("set storage: %w", err)
	}
	if err := tx.Set(KeyMessages, []byte(strconv.FormatInt(u.Messages, 10))); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("set messages: %w", err)
	}
	res, err := tx.Commit()
	if err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	if res != dict.CommitOK {
		return fmt.Errorf("commit result %v", res)
	}
	return nil
}
