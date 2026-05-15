package lmtp

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// deliverOne saves a single message to a pre-opened UserMailbox/UserIndex pair.
// The caller is responsible for opening handles via MailboxBackend.OpenUser and
// IndexBackend.OpenUser (after resolving the recipient's UserInfo), and for
// calling Init() on the UserMailbox before the first delivery in a session.
//
// Phase 4 — userdb lookup for LMTP:
//
//	Currently the resolver uses only the template (no userdb home override),
//	because LMTP delivery is unauthenticated and yarilo has no userdb lookup
//	path for incoming SMTP recipients. To support per-user home overrides
//	during delivery (matching Dovecot's lda/lmtp behaviour), add a UserDB
//	interface (driver: SQL query or dict protocol) and call it here before
//	OpenUser, passing the resulting home as homeOverride to Resolver.UserInfo.
func deliverOne(box mailbox.UserMailbox, idx mailbox.UserIndex, folder string, r io.ReadSeeker, size int64) error {
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
	if err := idx.AppendMessage(f.ID, meta); err != nil {
		_ = box.Remove(folder, filename)
		return fmt.Errorf("lmtp: index append: %w", err)
	}

	slog.Info("lmtp: delivered", "folder", folder, "uid", meta.UID, "size", size)
	return nil
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
