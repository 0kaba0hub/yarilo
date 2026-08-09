package jmap

import (
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/msgcache"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
)

// openEnvelopeCache opens the folder's cache file. A nil *Handle is a working
// value meaning "no cache", so every caller degrades to parsing rather than to
// an error.
func (s *Server) openEnvelopeCache(h *userHandle, ref messageRef) *msgcache.Handle {
	if h.idx == nil || s.opts.Storage == nil {
		return nil
	}
	return msgcache.Open(h.idx, ref.folderID, msgcache.Options{
		Locker: s.opts.Storage.Locker,
		User:   h.info.Username,
		Folder: ref.folder,
	})
}

// fillFromEnvelope answers the envelope-derived properties from a cached
// ENVELOPE, which is what lets a mailbox listing skip opening every message.
//
// It must agree with fillHeaders on the same message: the envelope was parsed
// from the same header block, so the difference is only where the parse
// happened. Fields ENVELOPE does not carry -- References, Headers -- stay
// empty, and EnvelopeSuffices refuses a request that names them.
func fillFromEnvelope(email *jmapcore.Email, env *imaplib.Envelope) {
	if s := env.Subject; s != "" {
		email.Subject = &s
	}
	if !env.Date.IsZero() {
		s := env.Date.UTC().Format(time.RFC3339)
		email.SentAt = &s
	}
	email.MessageID = envMessageIDs(env.MessageID)
	email.InReplyTo = envInReplyTo(env.InReplyTo)
	email.From = envAddresses(env.From)
	email.Sender = envAddresses(env.Sender)
	email.To = envAddresses(env.To)
	email.CC = envAddresses(env.Cc)
	email.BCC = envAddresses(env.Bcc)
	email.ReplyTo = envAddresses(env.ReplyTo)
}

// envMessageIDs mirrors messageIDs: JMAP carries bare ids without the angle
// brackets (§4.1.2.4), and an absent header is null rather than an empty list.
func envMessageIDs(v string) []string {
	return messageIDs(v)
}

func envInReplyTo(ids []string) []string {
	var out []string
	for _, id := range ids {
		out = append(out, messageIDs(id)...)
	}
	return out
}

// envAddresses converts ENVELOPE addresses to the JMAP form. Encoded words are
// already decoded: ExtractEnvelope built these through mail.Header, so decoding
// again would corrupt a name that literally contains "=?".
func envAddresses(addrs []imaplib.Address) []jmapcore.EmailAddress {
	if len(addrs) == 0 {
		return nil
	}
	out := make([]jmapcore.EmailAddress, 0, len(addrs))
	for _, a := range addrs {
		addr := jmapcore.EmailAddress{Email: a.Addr()}
		if a.Name != "" {
			name := a.Name
			addr.Name = &name
		}
		out = append(out, addr)
	}
	return out
}
