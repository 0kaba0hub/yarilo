package lmtp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// Deliverer is a thin wrapper used by the SMTP MX server for in-process local delivery.
// It reuses the same delivery logic as the LMTP server without the network overhead.
type Deliverer struct {
	mb  mailbox.MailboxBackend
	idx mailbox.IndexBackend
}

// NewDeliverer creates a Deliverer for use by the SMTP MX server.
func NewDeliverer(mb mailbox.MailboxBackend, idx mailbox.IndexBackend) *Deliverer {
	return &Deliverer{mb: mb, idx: idx}
}

// DeliverResult holds the per-recipient delivery outcome.
type DeliverResult struct {
	Rcpt string
	Err  error
}

// Deliver delivers msg to each recipient. Returns one result per recipient.
func (d *Deliverer) Deliver(ctx context.Context, from string, rcpts []string, msg io.Reader) []DeliverResult {
	data, err := io.ReadAll(msg)
	if err != nil {
		err = fmt.Errorf("lmtp/deliver: read: %w", err)
		results := make([]DeliverResult, len(rcpts))
		for i, r := range rcpts {
			results[i] = DeliverResult{Rcpt: r, Err: err}
		}
		return results
	}

	received := buildReceivedHeader(from)
	full := append([]byte(received), data...)

	results := make([]DeliverResult, len(rcpts))
	for i, rcpt := range rcpts {
		err := deliverOne(d.mb, d.idx, rcpt, bytes.NewReader(full), int64(len(full)))
		if err != nil {
			slog.Error("lmtp: delivery failed", "rcpt", rcpt, "err", err)
		}
		results[i] = DeliverResult{Rcpt: rcpt, Err: err}
	}
	return results
}

// deliverOne saves a single message to the recipient's mailbox and updates the index.
func deliverOne(mb mailbox.MailboxBackend, idx mailbox.IndexBackend, rcpt string, r io.ReadSeeker, size int64) error {
	user, folder, err := resolveMailbox(rcpt)
	if err != nil {
		return fmt.Errorf("lmtp: resolve %q: %w", rcpt, err)
	}

	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("lmtp: seek: %w", err)
	}
	data, _ := io.ReadAll(r)

	filename, err := mb.Save(user, folder, bytes.NewReader(data), size, nil)
	if err != nil {
		return fmt.Errorf("lmtp: save for %q: %w", rcpt, err)
	}

	f, err := idx.OpenFolder(user, folder, 0)
	if err != nil {
		_ = mb.Remove(user, folder, filename)
		return fmt.Errorf("lmtp: open index for %q: %w", rcpt, err)
	}

	modseq, err := idx.NextModSeq(f.ID)
	if err != nil {
		_ = mb.Remove(user, folder, filename)
		return fmt.Errorf("lmtp: modseq for %q: %w", rcpt, err)
	}

	meta := &mailbox.MessageMeta{
		Filename: filename,
		ModSeq:   modseq,
		Size:     uint32(size),
	}
	if err := idx.AppendMessage(f.ID, meta); err != nil {
		_ = mb.Remove(user, folder, filename)
		return fmt.Errorf("lmtp: index append for %q: %w", rcpt, err)
	}

	slog.Info("lmtp: delivered", "rcpt", rcpt, "uid", meta.UID, "size", size)
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

// resolveMailbox maps RCPT TO address to (user, folder).
// user+folder@domain → user=user@domain, folder=folder; otherwise folder=INBOX.
func resolveMailbox(rcpt string) (user, folder string, err error) {
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
