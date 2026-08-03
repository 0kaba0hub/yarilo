package jmap

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// specialUseRoles maps the IMAP special-use attribute (RFC 6154) to the JMAP
// role (RFC 8621 §2). The two registries name the same purposes differently, so
// one table is the whole translation.
var specialUseRoles = map[string]string{
	`\All`:       jmapcore.RoleAll,
	`\Archive`:   jmapcore.RoleArchive,
	`\Drafts`:    jmapcore.RoleDrafts,
	`\Flagged`:   jmapcore.RoleFlagged,
	`\Important`: jmapcore.RoleImportant,
	`\Junk`:      jmapcore.RoleJunk,
	`\Sent`:      jmapcore.RoleSent,
	`\Trash`:     jmapcore.RoleTrash,
}

// personalRights is the right set for a mailbox in the user's own namespace.
// Shared and public namespaces carry ACLs and arrive with the namespace phase;
// until then this reports what a personal mailbox actually allows rather than
// guessing at a restriction that is not enforced anywhere.
var personalRights = jmapcore.MailboxRights{
	MayReadItems:   true,
	MayAddItems:    true,
	MayRemoveItems: true,
	MaySetSeen:     true,
	MaySetKeywords: true,
	MayCreateChild: true,
	MayRename:      true,
	MayDelete:      true,
	MaySubmit:      true,
}

// mailboxList builds every mailbox the user has, in a stable order.
func (s *Server) mailboxList(h *userHandle) ([]jmapcore.Mailbox, error) {
	entries, err := h.box.ListFolders()
	if err != nil {
		return nil, fmt.Errorf("jmap: list folders: %w", err)
	}
	subscribed, err := h.subs.Snapshot()
	if err != nil {
		return nil, fmt.Errorf("jmap: subscriptions: %w", err)
	}
	// Snapshot carries only the per-user overrides; the configured defaults are
	// a second layer, exactly as Store.Get resolves them. Reading one layer
	// would drop every role an operator set in imap_special_use_defaults.
	overrides, err := h.specialUse.Snapshot()
	if err != nil {
		return nil, fmt.Errorf("jmap: special-use: %w", err)
	}
	roles := h.specialUse.Defaults()
	for folder, attr := range overrides {
		roles[folder] = attr
	}

	sep := separator(h.info)
	// A folder's id must be stable across a rename, so it is the folder GUID
	// from the index, not its name. That is the same identity METADATA and ACL
	// state are keyed by.
	ids := make(map[string]string, len(entries))
	folders := make(map[string]*mailbox.Folder, len(entries))
	for _, e := range entries {
		if !e.Selectable {
			// A \NoSelect container holds no mail and cannot be opened. It still
			// has to appear, or its children would have a parent id naming a
			// mailbox the client never saw.
			ids[e.Name] = containerID(e.Name)
			continue
		}
		f, err := h.idx.OpenFolder(e.Name, 0)
		if err != nil {
			return nil, fmt.Errorf("jmap: open folder %q: %w", e.Name, err)
		}
		ids[e.Name] = mailboxID(f.GUID)
		folders[e.Name] = f
	}

	// Sorted by full path so the response order is repeatable: the id is a
	// GUID, so nothing else in it would give a client a stable sequence.
	paths := make([]string, 0, len(entries))
	selectable := make(map[string]bool, len(entries))
	for _, e := range entries {
		paths = append(paths, e.Name)
		selectable[e.Name] = e.Selectable
	}
	sort.Strings(paths)

	out := make([]jmapcore.Mailbox, 0, len(paths))
	for _, path := range paths {
		mb := jmapcore.Mailbox{
			ID:           ids[path],
			Name:         leafName(path, sep),
			ParentID:     parentID(path, sep, ids),
			Role:         roleOf(path, roles),
			MyRights:     personalRights,
			IsSubscribed: contains(subscribed, path),
		}
		if f := folders[path]; f != nil {
			mb.TotalEmails = f.Messages
			mb.UnreadEmails = f.Unseen
			// Threads are not computed yet: until the Thread phase every message
			// is its own thread, so the counts coincide. Reporting 0 would be a
			// lie a client acts on; the message counts are the truth under a
			// one-message-per-thread model.
			mb.TotalThreads = f.Messages
			mb.UnreadThreads = f.Unseen
		}
		if !selectable[path] {
			mb.MyRights.MayReadItems = false
			mb.MyRights.MayAddItems = false
			mb.MyRights.MayRemoveItems = false
			mb.MyRights.MaySubmit = false
		}
		out = append(out, mb)
	}
	return out, nil
}

// mailboxID is the id a client sees for a mailbox: the same string IMAP reports
// as MAILBOXID (RFC 8474), through the same formatter, so the two protocols
// name one mailbox identically by construction rather than by coincidence.
func mailboxID(guid [16]byte) string {
	return mailbox.FormatObjectID(guid)
}

// containerID names a \NoSelect node, which has no index entry and therefore no
// GUID. The prefix keeps it from ever colliding with a hex GUID.
func containerID(name string) string { return "container:" + name }

func separator(info *mailbox.UserInfo) string {
	if info != nil && info.Separator != "" {
		return info.Separator
	}
	return "/"
}

func leafName(path, sep string) string {
	if i := strings.LastIndex(path, sep); i >= 0 {
		return path[i+len(sep):]
	}
	return path
}

// parentID resolves the containing mailbox. A top-level mailbox has none, which
// is null on the wire rather than an empty string.
func parentID(path, sep string, ids map[string]string) *string {
	i := strings.LastIndex(path, sep)
	if i < 0 {
		return nil
	}
	id, ok := ids[path[:i]]
	if !ok {
		return nil
	}
	return &id
}

// roleOf translates the mailbox's special-use attribute. INBOX carries no
// attribute in IMAP but is the inbox role in JMAP.
func roleOf(name string, roles map[string]imaplib.MailboxAttr) *string {
	if strings.EqualFold(name, "INBOX") {
		role := jmapcore.RoleInbox
		return &role
	}
	attr, ok := roles[name]
	if !ok {
		return nil
	}
	role, ok := specialUseRoles[string(attr)]
	if !ok {
		return nil
	}
	return &role
}

func contains(set map[string]struct{}, name string) bool {
	_, ok := set[name]
	return ok
}

// mailboxState digests the whole mailbox set. It is what Mailbox/get returns as
// "state" and Mailbox/query as "queryState": a value that changes whenever any
// mailbox the client can see changes, and stays put otherwise. It is not a
// change log — canCalculateChanges is false until Mailbox/changes lands, so a
// client refetches rather than diffing.
func mailboxState(list []jmapcore.Mailbox) string {
	h := sha256.New()
	for _, mb := range list {
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00%d\x00%t\x00", mb.ID, mb.Name,
			mb.TotalEmails, mb.UnreadEmails, mb.IsSubscribed)
		if mb.Role != nil {
			h.Write([]byte(*mb.Role)) //nolint:errcheck // hash writes cannot fail
		}
		h.Write([]byte{0}) //nolint:errcheck
		if mb.ParentID != nil {
			h.Write([]byte(*mb.ParentID)) //nolint:errcheck
		}
		h.Write([]byte{0}) //nolint:errcheck
	}
	// Deliberately not FormatObjectID: this is a digest of the set, not an
	// object id. Routing it through the identity formatter would tie a state
	// string to an identity format it has nothing to do with.
	return hex.EncodeToString(h.Sum(nil)[:8])
}
