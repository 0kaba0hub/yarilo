package jmap

import (
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/mail"
	"strings"
	"time"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // registers the charset decoders

	"github.com/yarilomail/yarilo/pkg/jmapcore"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// previewChars is the length of the preview string (RFC 8621 §4.1.4 leaves it
// to the server; 256 is what the major implementations settled on).
const previewChars = 256

// messageRef locates one message: JMAP ids are global to the account, but the
// store addresses a message by folder and UID.
type messageRef struct {
	folder   string
	folderID uint64
	meta     *mailbox.MessageMeta
	// mailboxID is the JMAP id of the folder the message was found in.
	mailboxID string
}

// emailID is the message GUID, which is also its EMAILID in IMAP (RFC 8474).
// One identity across both protocols is the point, so it goes through the same
// formatter IMAP uses: encoding it here as well would make the match a
// coincidence of implementation that a format change would silently break.
func emailID(m *mailbox.MessageMeta) string {
	return mailbox.FormatObjectID(m.GUID)
}

// findMessages walks the user's folders for the requested ids. Nothing indexes
// GUID to folder yet, so this is a scan; it stops as soon as every id is found
// so the common case of a handful of ids does not read every folder.
func (s *Server) findMessages(h *userHandle, want map[string]bool) (map[string]messageRef, error) {
	entries, err := h.box.ListFolders()
	if err != nil {
		return nil, fmt.Errorf("jmap: list folders: %w", err)
	}
	found := make(map[string]messageRef, len(want))
	for _, e := range entries {
		if !e.Selectable {
			continue
		}
		f, err := h.idx.OpenFolder(e.Name, 0)
		if err != nil {
			return nil, fmt.Errorf("jmap: open folder %q: %w", e.Name, err)
		}
		metas, err := h.idx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
		if err != nil {
			return nil, fmt.Errorf("jmap: read folder %q: %w", e.Name, err)
		}
		mboxID := mailboxID(f.GUID)
		for _, m := range metas {
			id := emailID(m)
			if want != nil && !want[id] {
				continue
			}
			if _, seen := found[id]; seen {
				continue
			}
			found[id] = messageRef{folder: e.Name, folderID: f.ID, meta: m, mailboxID: mboxID}
		}
		if want != nil && len(found) == len(want) {
			break
		}
	}
	return found, nil
}

// buildEmail renders one Email. Bodies are read only when a body value or a
// structural property was actually asked for: an Email/get for envelope fields
// must not touch the message at all.
func (s *Server) buildEmail(h *userHandle, ref messageRef, req jmapcore.EmailGetRequest, ceiling uint32) (jmapcore.Email, error) {
	m := ref.meta
	email := jmapcore.Email{
		ID:         emailID(m),
		BlobID:     emailID(m),
		ThreadID:   emailID(m), // one message per thread until threading lands
		MailboxIDs: map[string]bool{ref.mailboxID: true},
		Keywords:   keywordsOf(m),
		Size:       m.Size,
		ReceivedAt: m.InternalDate.UTC().Format(time.RFC3339),
		BodyValues: map[string]jmapcore.EmailBodyValue{},
	}

	// Nothing below is answerable from the index, so a request that named only
	// index-backed properties never opens the message.
	if !req.NeedsMessage() {
		return email, nil
	}

	rc, err := h.box.Fetch(ref.folder, m.Filename, m.AltTier)
	if err != nil {
		return email, fmt.Errorf("jmap: fetch %s/%d: %w", ref.folder, m.UID, err)
	}
	defer rc.Close() //nolint:errcheck

	// A message that cannot be parsed still exists: Email/query lists it,
	// download serves its bytes and IMAP shows it. Reporting notFound here
	// would leave a client unable to reconcile its own view of the account
	// (#1001), so the object comes back with the half that is trustworthy —
	// everything the index carries — and the header-derived properties stay
	// empty. Malformed headers are ordinary in real mail; this is not an
	// exceptional path.
	//
	// A Fetch failure above is the other condition and keeps its own answer:
	// there the store cannot produce the message at all, download 404s too, and
	// the methods agree that it is absent.
	entity, err := message.Read(rc)
	if err != nil && entity == nil {
		slog.Warn("jmap: message could not be parsed, serving index-derived properties only",
			"folder", ref.folder, "uid", m.UID, "id", email.ID, "err", err)
		return email, nil
	}
	fillHeaders(&email, entity.Header)

	// A request that named only header properties stops here. Walking the MIME
	// tree is what pulls the message body off disk and decodes it, so for the
	// commonest request a client makes — subject and sender for every row of a
	// mailbox listing — the walk was the entire cost and none of the answer.
	if !req.NeedsStructure() {
		return email, nil
	}

	parts := collectParts(entity, "")
	for _, p := range parts {
		switch {
		case p.disposition == "attachment" || (p.filename != "" && !strings.HasPrefix(p.mediaType, "text/")):
			email.Attachments = append(email.Attachments, p.part)
			email.HasAttachment = true
		case p.mediaType == "text/plain":
			email.TextBody = append(email.TextBody, p.part)
		case p.mediaType == "text/html":
			email.HTMLBody = append(email.HTMLBody, p.part)
		}
	}

	// The preview comes from the first text part, which is already decoded, so
	// it costs nothing beyond what the walk above did.
	for _, p := range parts {
		if p.mediaType == "text/plain" && p.body != "" {
			email.Preview = previewOf(p.body)
			break
		}
	}

	if req.WantsBodyValues() {
		limit := jmapcore.EffectiveBodyBytes(req.MaxBodyValueBytes, ceiling)
		for _, p := range parts {
			if !wantsPart(req, p.mediaType) || p.part.PartID == nil {
				continue
			}
			value, truncated := jmapcore.TruncateBody(p.body, limit)
			if p.mediaType == "text/html" {
				value, truncated = jmapcore.TruncateHTML(p.body, limit)
			}
			email.BodyValues[*p.part.PartID] = jmapcore.EmailBodyValue{
				Value:             value,
				IsEncodingProblem: p.encodingProblem,
				IsTruncated:       truncated,
			}
		}
	}
	return email, nil
}

func wantsPart(req jmapcore.EmailGetRequest, mediaType string) bool {
	switch {
	case req.FetchAllBodyValues:
		return true
	case req.FetchTextBodyValues && mediaType == "text/plain":
		return true
	case req.FetchHTMLBodyValues && mediaType == "text/html":
		return true
	}
	return false
}

// walkedPart is one MIME part plus what building the Email needs from it.
type walkedPart struct {
	part            jmapcore.EmailBodyPart
	mediaType       string
	disposition     string
	filename        string
	body            string
	encodingProblem bool
}

// collectParts flattens the MIME tree, numbering parts as RFC 8621 §4.1.4
// requires: "1", "1.1", "2" and so on.
func collectParts(e *message.Entity, prefix string) []walkedPart {
	mediaType, params, err := mime.ParseMediaType(e.Header.Get("Content-Type"))
	if err != nil {
		mediaType = "text/plain"
		params = map[string]string{}
	}
	if mr := e.MultipartReader(); mr != nil {
		var out []walkedPart
		for i := 1; ; i++ {
			sub, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				break
			}
			id := fmt.Sprintf("%d", i)
			if prefix != "" {
				id = prefix + "." + id
			}
			out = append(out, collectParts(sub, id)...)
		}
		return out
	}

	partID := prefix
	if partID == "" {
		partID = "1"
	}
	disposition, dispParams, _ := mime.ParseMediaType(e.Header.Get("Content-Disposition"))
	filename := dispParams["filename"]
	if filename == "" {
		filename = params["name"]
	}

	body, readErr := io.ReadAll(e.Body)
	p := walkedPart{
		mediaType:       mediaType,
		disposition:     disposition,
		filename:        filename,
		body:            string(body),
		encodingProblem: readErr != nil,
		part: jmapcore.EmailBodyPart{
			PartID: &partID,
			Size:   uint32(len(body)),
			Type:   mediaType,
		},
	}
	if charset := params["charset"]; charset != "" {
		p.part.Charset = &charset
	}
	if filename != "" {
		p.part.Name = &filename
	}
	if disposition != "" {
		p.part.Disposition = &disposition
	}
	if cid := strings.Trim(e.Header.Get("Content-Id"), "<>"); cid != "" {
		p.part.CID = &cid
	}
	return []walkedPart{p}
}

// fillHeaders lifts the envelope fields (RFC 8621 §4.1.2).
func fillHeaders(email *jmapcore.Email, h message.Header) {
	if s := h.Get("Subject"); s != "" {
		decoded := decodeWord(s)
		email.Subject = &decoded
	}
	if d := h.Get("Date"); d != "" {
		if t, err := mail.ParseDate(d); err == nil {
			s := t.UTC().Format(time.RFC3339)
			email.SentAt = &s
		}
	}
	email.MessageID = messageIDs(h.Get("Message-Id"))
	email.InReplyTo = messageIDs(h.Get("In-Reply-To"))
	email.References = messageIDs(h.Get("References"))
	email.From = addresses(h.Get("From"))
	email.Sender = addresses(h.Get("Sender"))
	email.To = addresses(h.Get("To"))
	email.CC = addresses(h.Get("Cc"))
	email.BCC = addresses(h.Get("Bcc"))
	email.ReplyTo = addresses(h.Get("Reply-To"))
}

// messageIDs splits a header of angle-addr tokens, stripping the brackets: JMAP
// carries the bare ids (§4.1.2.4).
func messageIDs(v string) []string {
	if v == "" {
		return nil
	}
	var out []string
	for _, f := range strings.Fields(v) {
		if id := strings.Trim(f, "<>,"); id != "" {
			out = append(out, id)
		}
	}
	return out
}

func addresses(v string) []jmapcore.EmailAddress {
	if v == "" {
		return nil
	}
	list, err := mail.ParseAddressList(v)
	if err != nil {
		// An unparseable header is reported as a single address rather than
		// dropped: a client showing the raw value beats showing nothing.
		return []jmapcore.EmailAddress{{Email: v}}
	}
	out := make([]jmapcore.EmailAddress, 0, len(list))
	for _, a := range list {
		addr := jmapcore.EmailAddress{Email: a.Address}
		if a.Name != "" {
			name := a.Name
			addr.Name = &name
		}
		out = append(out, addr)
	}
	return out
}

func decodeWord(s string) string {
	dec := new(mime.WordDecoder)
	if out, err := dec.DecodeHeader(s); err == nil {
		return out
	}
	return s
}

// previewOf renders the plain-text preview: collapsed whitespace, cut on a rune
// boundary like every other truncation here.
func previewOf(body string) string {
	preview := strings.Join(strings.Fields(body), " ")
	out, _ := jmapcore.TruncateBody(preview, previewChars)
	return out
}

func keywordsOf(m *mailbox.MessageMeta) map[string]bool {
	// IMAP system flags map to the JMAP keywords of RFC 8621 §4.1.1.
	systemFlags := map[string]string{
		`\Seen`:     "$seen",
		`\Answered`: "$answered",
		`\Flagged`:  "$flagged",
		`\Draft`:    "$draft",
		`\Deleted`:  "$deleted",
	}
	out := make(map[string]bool, len(m.Flags)+len(m.Keywords))
	for _, f := range m.Flags {
		if kw, ok := systemFlags[f]; ok {
			out[kw] = true
		}
	}
	for _, kw := range m.Keywords {
		out[strings.ToLower(kw)] = true
	}
	return out
}
