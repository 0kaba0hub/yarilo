package imap

import (
	"context"
	"fmt"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// resolveOwnerUserInfo resolves the owner of an owner-templated namespace
// instance through the userdb, then stamps the storage identity via the shared
// producer (mailbox.StampOwnerLocation, precedence in https://doc.yarilomail.org/OWNER_SHARED_NS
// 3.3). A userdb miss is a NO [NONEXISTENT] to the caller -- no such user, no
// such owner.
func resolveOwnerUserInfo(
	ctx context.Context,
	lookup func(context.Context, string) (*mailbox.UserInfo, error),
	base *mailbox.UserInfo,
	spec NamespaceSpec,
	owner string,
) (*mailbox.UserInfo, error) {
	if lookup == nil {
		return nil, fmt.Errorf("imap: owner resolution unavailable (no userdb lookup wired)")
	}
	if owner == "" {
		return nil, fmt.Errorf("imap: empty owner")
	}
	found, err := lookup(ctx, owner)
	if err != nil || found == nil {
		// A userdb miss is the answer: no such user, so no such owner.
		return nil, fmt.Errorf("imap: owner %q not found: %w", owner, err)
	}
	// One producer for the precedence; the lookup above is this path's own.
	return mailbox.StampOwnerLocation(found, base, spec.Location, byte(spec.Separator))
}
