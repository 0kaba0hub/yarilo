package lmtp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/locks"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// deliverOne saves a single message into the recipient's folder. The
// caller opens handles via MailboxBackend.OpenUser + IndexBackend.OpenUser
// (after resolving the recipient's UserInfo) and calls Init() on the
// UserMailbox before the first delivery in a session.
//
// When locker is non-nil and the delivery succeeds, a `delivered` EVENT
// is emitted on mbox:<username>:<folder> so any subscribed IMAP IDLE
// session (in this or any other pod) is woken up. Username is used only
// to build the lock/event key — the actual mailbox path is resolved
// upstream via the per-user UserMailbox handle.
//
// Phase 4 — userdb lookup for LMTP:
//
//	Currently the resolver uses only the template (no userdb home override),
//	because LMTP delivery is unauthenticated and yarilo has no userdb lookup
//	path for incoming SMTP recipients. To support per-user home overrides
//	during delivery (matching Dovecot's lda/lmtp behaviour), add a UserDB
//	interface (driver: SQL query or dict protocol) and call it here before
//	OpenUser, passing the resulting home as homeOverride to Resolver.UserInfo.
func deliverOne(box mailbox.UserMailbox, idx mailbox.UserIndex, folder string, r io.ReadSeeker, size int64, locker locks.Locker, username string) error {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("lmtp: seek: %w", err)
	}
	data, _ := io.ReadAll(r)

	filename, err := box.Save(folder, bytes.NewReader(data), size, nil)
	if err != nil {
		return fmt.Errorf("lmtp: save: %w", err)
	}

	f, err := idx.OpenFolder(folder, 0)
	if err != nil {
		_ = box.Remove(folder, filename)
		return fmt.Errorf("lmtp: open index: %w", err)
	}

	modseq, err := idx.NextModSeq(f.ID)
	if err != nil {
		_ = box.Remove(folder, filename)
		return fmt.Errorf("lmtp: modseq: %w", err)
	}

	meta := &mailbox.MessageMeta{
		Filename: filename,
		ModSeq:   modseq,
		Size:     uint32(size),
	}
	uid, err := idx.AllocateAppend(f.ID, meta)
	if err != nil {
		_ = box.Remove(folder, filename)
		return fmt.Errorf("lmtp: index allocate-append: %w", err)
	}
	if err := box.AppendUIDEntry(folder, uid, filename); err != nil {
		// The index already holds the record under uid; the uidlist entry
		// is a Dovecot-compat artefact used by Maildir for fast UID lookup.
		// Leaving it inconsistent would cause subsequent List() to skip the
		// message. Surface the error so the LMTP transaction reports DEFER
		// and the upstream retries.
		_ = box.Remove(folder, filename)
		return fmt.Errorf("lmtp: uidlist append: %w", err)
	}

	emitMailboxEvent(locker, username, folder, locks.EventDelivered, uid)
	slog.Info("lmtp: delivered", "folder", folder, "uid", uid, "size", size)
	return nil
}

// emitMailboxEvent is a best-effort fire-and-forget publish. Errors are
// logged at debug level and never surfaced — events are advisory, the
// authoritative state lives in the index file. A 1-second timeout avoids
// blocking a delivery if the locks server is sluggish.
func emitMailboxEvent(locker locks.Locker, username, folder string, eventType locks.EventType, uid uint32) {
	if locker == nil || username == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	payload := strconv.FormatUint(uint64(uid), 10)
	if err := locker.Emit(ctx, locks.MailboxKey(username, folder), eventType, payload); err != nil {
		slog.Debug("lmtp: emit event failed",
			"folder", folder, "type", string(eventType), "err", err)
	}
}

// stripDetail removes the +detail part from an address: user+tag@domain → user@domain.
func stripDetail(addr string) string {
	addr = strings.TrimSpace(strings.Trim(addr, "<>"))
	at := strings.LastIndex(addr, "@")
	if at < 0 {
		return addr
	}
	local, domain := addr[:at], addr[at+1:]
	if plus := strings.Index(local, "+"); plus >= 0 {
		local = local[:plus]
	}
	return local + "@" + domain
}

// resolveMailbox maps RCPT TO address to (username, folder).
// user+folder@domain → username=user@domain, folder=folder; otherwise folder=INBOX.
func resolveMailbox(rcpt string) (username, folder string, err error) {
	rcpt = strings.TrimSpace(strings.Trim(rcpt, "<>"))
	at := strings.LastIndex(rcpt, "@")
	if at < 0 {
		return "", "", fmt.Errorf("no @ in rcpt %q", rcpt)
	}
	local := rcpt[:at]
	domain := rcpt[at+1:]

	folder = "INBOX"
	if plus := strings.Index(local, "+"); plus >= 0 {
		folder = local[plus+1:]
		local = local[:plus]
	}
	return local + "@" + domain, folder, nil
}

func buildReceivedHeader(from string) string {
	return fmt.Sprintf("Received: from %s by yarilo with LMTP; %s\r\n",
		from, time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 +0000"))
}
