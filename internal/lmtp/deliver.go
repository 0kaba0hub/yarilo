// Package lmtp implements local LMTP delivery to a MailboxBackend + IndexBackend.
// It is invoked by the SMTP inbound server after anti-spam/auth checks pass.
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

// Deliverer delivers messages to local mailboxes.
type Deliverer struct {
	mb  mailbox.MailboxBackend
	idx mailbox.IndexBackend
}

// New creates a Deliverer backed by mb and idx.
func New(mb mailbox.MailboxBackend, idx mailbox.IndexBackend) *Deliverer {
	return &Deliverer{mb: mb, idx: idx}
}

// DeliverResult holds the per-recipient delivery outcome.
type DeliverResult struct {
	Rcpt string
	Err  error
}

// Deliver delivers msg to each recipient in rcpts.
// msg is buffered so it can be delivered to multiple recipients.
// Returns one result per recipient; a nil Err means success.
func (d *Deliverer) Deliver(ctx context.Context, from string, rcpts []string, msg io.Reader) []DeliverResult {
	data, err := io.ReadAll(msg)
	if err != nil {
		err = fmt.Errorf("lmtp/deliver: read message: %w", err)
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
		results[i] = DeliverResult{
			Rcpt: rcpt,
			Err:  d.deliverOne(ctx, rcpt, bytes.NewReader(full)),
		}
	}
	return results
}

func (d *Deliverer) deliverOne(ctx context.Context, rcpt string, r io.ReadSeeker) error {
	user, folder, err := resolveMailbox(rcpt)
	if err != nil {
		return fmt.Errorf("lmtp/deliver: resolve %q: %w", rcpt, err)
	}

	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("lmtp/deliver: seek: %w", err)
	}
	data, _ := io.ReadAll(r)
	size := int64(len(data))

	filename, err := d.mb.Save(user, folder, bytes.NewReader(data), size, nil)
	if err != nil {
		return fmt.Errorf("lmtp/deliver: save for %q: %w", rcpt, err)
	}

	f, err := d.idx.OpenFolder(user, folder, 0)
	if err != nil {
		_ = d.mb.Remove(user, folder, filename)
		return fmt.Errorf("lmtp/deliver: open index for %q: %w", rcpt, err)
	}

	modseq, err := d.idx.NextModSeq(f.ID)
	if err != nil {
		_ = d.mb.Remove(user, folder, filename)
		return fmt.Errorf("lmtp/deliver: modseq for %q: %w", rcpt, err)
	}

	meta := &mailbox.MessageMeta{
		Filename: filename,
		Flags:    nil,
		ModSeq:   modseq,
		Size:     uint32(size),
	}
	if err := d.idx.AppendMessage(f.ID, meta); err != nil {
		_ = d.mb.Remove(user, folder, filename)
		return fmt.Errorf("lmtp/deliver: index append for %q: %w", rcpt, err)
	}

	slog.Info("lmtp: delivered", "rcpt", rcpt, "uid", meta.UID, "size", size)
	return nil
}

// resolveMailbox maps an RCPT address to (user, folder).
// Format: user@domain → user=user@domain, folder=INBOX
// Format: user+folder@domain → user=user@domain, folder=folder
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
	user = local + "@" + domain
	return user, folder, nil
}

func buildReceivedHeader(from string) string {
	return fmt.Sprintf("Received: from %s by yarilo with LMTP; %s\r\n",
		from, time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 +0000"))
}
