package lmtp

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// deliverCallSeq tags each deliverOne call with a process-local, monotonically
// increasing id so concurrent deliveries racing on the same folder can be told
// apart in the shared debug log stream (see the "lmtp: uid allocated" /
// "lmtp: uid committed" breadcrumbs below).
var deliverCallSeq atomic.Uint64

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
//	during delivery, add a UserDB interface (driver: SQL query or dict
//	protocol) and call it here before OpenUser, passing the resulting home
//	as homeOverride to Resolver.UserInfo.
//
// deliverOne returns the delivered UID and the folder it landed in. The folder
// travels back because the full-text hook needs its GUID: the index is keyed by
// it (#1183), and a reference built from the name alone is refused by the
// service -- silently, on a fire-and-forget path (#1206 found it).
func deliverOne(box mailbox.UserMailbox, idx mailbox.UserIndex, folder string, r io.ReadSeeker, size int64, locker locks.Locker, username, from string, flags []string) (uint32, mailbox.Folder, [16]byte, error) {
	tDeliver := time.Now()
	var noGUID [16]byte
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return 0, mailbox.Folder{}, noGUID, fmt.Errorf("lmtp: seek: %w", err)
	}
	data, _ := io.ReadAll(r)

	f, err := idx.OpenFolder(folder, 0)
	if err != nil {
		return 0, mailbox.Folder{}, noGUID, fmt.Errorf("lmtp: open index: %w", err)
	}
	uid, err := idx.AllocateUID(f.ID)
	if err != nil {
		return 0, *f, noGUID, fmt.Errorf("lmtp: allocate UID: %w", err)
	}
	// Breadcrumb for the non-atomic AllocateUID -> Save -> AppendMessage window:
	// AllocateUID commits and releases the folder lock immediately, so any other
	// delivery to the same folder can interleave here while this one is still
	// writing the body (mdbox/sdbox: map lookup + refcount + possible rotation,
	// measurably slower than maildir's flat-file write). Logged with the uid and
	// a per-call correlation id so two deliveries racing on the same folder can
	// be told apart in a shared log stream.
	callID := deliverCallSeq.Add(1)
	slog.Debug("lmtp: uid allocated", "user", username, "folder", folder, "uid", uid, "call_id", callID)
	modseq, err := idx.NextModSeq(f.ID)
	if err != nil {
		return 0, *f, noGUID, fmt.Errorf("lmtp: modseq: %w", err)
	}
	tSave := time.Now()
	filename, vsize, guid, err := box.Save(folder, bytes.NewReader(data), uid, size, flags, [16]byte{})
	if err != nil {
		return 0, *f, noGUID, fmt.Errorf("lmtp: save: %w", err)
	}
	tIndex := time.Now()
	slog.Debug("lmtp: body saved, committing index", "user", username, "folder", folder, "uid", uid,
		"call_id", callID, "filename", filename, "save_ms", tIndex.Sub(tSave).Milliseconds())
	if err := idx.AppendMessage(f.ID, &mailbox.MessageMeta{
		UID:          uid,
		Filename:     filename,
		ModSeq:       modseq,
		Size:         uint32(size),
		VSize:        vsize,
		InternalDate: time.Now(),
		Flags:        flags,
		GUID:         guid,
	}); err != nil {
		slog.Warn("lmtp: index append failed, rolling back save",
			"user", username, "folder", folder, "uid", uid, "call_id", callID, "err", err)
		_ = box.Remove(folder, filename)
		return 0, *f, noGUID, fmt.Errorf("lmtp: index append: %w", err)
	}
	slog.Debug("lmtp: uid committed", "user", username, "folder", folder, "uid", uid, "call_id", callID)
	slog.Debug("lmtp: deliver timing",
		"folder", folder, "size", size,
		"save_ms", tIndex.Sub(tSave).Milliseconds(),
		"index_ms", time.Since(tIndex).Milliseconds(),
		"total_ms", time.Since(tDeliver).Milliseconds())
	emitMailboxEvent(locker, username, folder, locks.EventDelivered, uid)
	slog.Info("lmtp: delivered", "from", from, "to", username, "folder", folder, "uid", uid, "file", filename, "size", size)
	return uid, *f, guid, nil
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

// buildReceivedHeader names the host that accepted the message. The name was a
// literal, for the same reason the Message-ID's was: nothing carried one
// (#1506).
func buildReceivedHeader(from, host string) string {
	if host == "" {
		host = "yarilo"
	}
	return fmt.Sprintf("Received: from %s by %s with LMTP; %s\r\n",
		from, host, time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 +0000"))
}

// hasMessageID reports whether the message already carries a Message-ID.
//
// The header section only, up to the blank line: a body can contain anything,
// including a quoted copy of another message's headers, and treating that as
// this message's identity would leave the real one missing on exactly the mail
// most likely to be a reply.
//
// Field names are case-insensitive (RFC 5322 §1.2.2), and only a line that
// starts at column zero begins a field -- a leading space or tab is the
// continuation of the one before it.
func hasMessageID(data []byte) bool {
	const name = "message-id:"
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		line := data
		if end >= 0 {
			line = data[:end]
			data = data[end+1:]
		} else {
			data = nil
		}
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			return false // end of the header section
		}
		if line[0] == ' ' || line[0] == '\t' {
			continue
		}
		if len(line) >= len(name) && strings.EqualFold(string(line[:len(name)]), name) {
			return true
		}
	}
	return false
}

// buildMessageID makes an identifier for a message that arrived without one.
//
// 128 bits of randomness, so it is unique among all messages rather than among
// the messages of one host or one run -- which is what RFC 5322 §3.6.4 asks
// for, and what a counter or a hash of the recipient would not give.
//
// The domain part is the hostname this LMTP server announces itself with. It is
// not read from the Received header, which carries a fixed literal rather than
// a configured name.
func buildMessageID(host string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any platform we run on, and a delivery
		// is not the place to decide what to do if it did.
		panic("lmtp: crypto/rand: " + err.Error())
	}
	if host == "" {
		host = "yarilo"
	}
	return fmt.Sprintf("Message-ID: <%x@%s>\r\n", b, host)
}
