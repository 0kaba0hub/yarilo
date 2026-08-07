package imap

import (
	"context"
	"fmt"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// resolveOwnerUserInfo builds the storage identity for the owner of an
// owner-templated namespace instance. It is a separate producer from
// NamespaceUserInfo on purpose: the fixed shared/public case takes its driver
// and modifiers from the namespace location, and this case takes them from the
// owner's userdb -- two different sources, so two producers rather than one
// with a switch (docs/OWNER_SHARED_NS.md 3.3, 3.5).
//
// The precedence, decided in 3.3 and implemented here as a fixed order rather
// than a property of statement sequence:
//
//   - the owner's userdb decides the driver and the modifiers it set -- the
//     lookup returns an owner UserInfo already carrying them (ResolveUserInfo
//     runs StampLocation for the owner exactly as it does for the session user);
//   - the root comes from the namespace location expanded against the owner;
//   - namespace-location modifiers fill only fields the userdb left empty
//     (first-writer-wins), so the userdb's win where it spoke.
//
// The driver is NOT rewritten after the lookup: it is already the owner's, and
// overwriting it with the namespace location's driver is the exact per-user
// driver bug the full lookup exists to prevent.
//
// Errors: the owner failing to resolve in the userdb is a NO [NONEXISTENT] to
// the caller -- a userdb miss is "no such user", and there is no owner to own
// anything. An empty root after expansion is refused, the same invariant
// NamespaceUserInfo holds.
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
	// Copy before mutating: this function overwrites Home/MailPath/Separator, and
	// a lookup that ever returns a shared pointer (a cache) would otherwise have
	// its entry rewritten under it. Safe regardless of what the lookup returns
	// -- the on-demand cache stores the resolved result, not this input.
	uiCopy := *found
	ownerUI := &uiCopy

	// Root from the namespace location, expanded against the owner.
	loc, ok, perr := mailbox.ParseLocation(spec.Location, ownerUI)
	if perr != nil {
		return nil, fmt.Errorf("imap: namespace location %q: %w", spec.Location, perr)
	}
	if !ok || loc.Path == "" {
		return nil, fmt.Errorf("imap: namespace location %q resolved to no path for owner %q", spec.Location, owner)
	}
	// Root from the owner's userdb when it gave one, the namespace template
	// only otherwise -- fillIfEmpty on the root, the same rule the session user
	// already follows (server.go: userdb MailPath wins when present). The lookup
	// set Home and MailPath to the owner's real store, and overwriting them with
	// the namespace template would point the owner's userdb driver at a
	// template path: for a deployment with per-user drivers (mdbox/maildir/sdbox
	// by account) one template cannot name all three roots, so it would open an
	// mdbox driver on a maildir tree -- the parallel tree this design prevents.
	// The template fills only an owner whose userdb gave no mail_location.
	fillIfEmpty(&ownerUI.MailPath, loc.Path)
	fillIfEmpty(&ownerUI.Home, loc.Path)
	ownerUI.Separator = string(spec.Separator)

	// Namespace-location modifiers fill only what the userdb left empty.
	fillIfEmpty(&ownerUI.IndexDir, loc.IndexDir)
	fillIfEmpty(&ownerUI.VolatileDir, loc.VolatileDir)
	fillIfEmpty(&ownerUI.ControlDir, loc.ControlDir)
	fillIfEmpty(&ownerUI.AltDir, loc.AltDir)

	// Storage-name form is a deployment-wide property, not the owner's: carry it
	// from the session base so the same mailbox is spelled the same on disk
	// whoever opens it (#1078, #1092).
	if base != nil {
		ownerUI.StorageEscapeChar = base.StorageEscapeChar
		ownerUI.SkipNFCNormalize = base.SkipNFCNormalize
	}
	// Username stays the owner's: the lookup set it, and it is the ACL subject
	// and lock owner for the owner's store.
	return ownerUI, nil
}

func fillIfEmpty(dst *string, src string) {
	if *dst == "" {
		*dst = src
	}
}
