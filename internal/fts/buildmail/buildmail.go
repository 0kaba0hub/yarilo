// Package buildmail turns a raw RFC 5322 message into the FTS build-key
// stream. It walks the MIME
// structure, emits KeyHeader / KeyMIMEHeader for indexable header fields and
// KeyBodyPart for decoded text parts (HTML converted to text, other
// attachment types routed through an optional external decoder), skips
// multipart containers, and caps the indexed body size.
package buildmail

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset"

	"github.com/0kaba0hub/yarilo/internal/fts/decoder"
	"github.com/0kaba0hub/yarilo/internal/fts/language"
	"github.com/0kaba0hub/yarilo/pkg/fts"
)

// Options selects which headers are indexed and how much body text is fed.
type Options struct {
	// HeaderIncludes / HeaderExcludes filter header fields by name
	// (case-insensitive; a trailing '*' is a prefix mask). The default is
	// index-unless-excluded; an include match overrides an exclude match.
	HeaderIncludes []string
	HeaderExcludes []string
	// MaxSize caps the total body bytes fed to the index per message
	// (fts_message_max_size). 0 = unlimited.
	MaxSize int64

	// Decoder extracts text from non-text/HTML attachment parts (PDF,
	// office documents, etc.). Nil = attachments beyond text/HTML stay
	// unindexed (fts_decoder_driver=none, the default). See #669.
	Decoder decoder.Decoder

	// DedupBodyParts skips re-tokenizing a body part whose normalized text
	// content was already indexed for the SAME message — multipart/alternative
	// text+html twins, or a quoted block repeated within one body. Opt-in
	// (fts_dedup_body_parts, default false): the extra per-part hashing and
	// buffering is skipped entirely on the default fast path. See #669.
	DedupBodyParts bool
}

// Builder streams messages into an fts.Update through one language chain.
type Builder struct {
	opts  Options
	chain *language.Chain
}

// New returns a Builder. The chain is required: the first engine is
// tokenized, so every part streams through the tokenizer + filters.
func New(opts Options, chain *language.Chain) *Builder {
	return &Builder{opts: opts, chain: chain}
}

func matchHeaderList(name string, list []string) bool {
	for _, pat := range list {
		if p, ok := strings.CutSuffix(pat, "*"); ok {
			if len(name) >= len(p) && strings.EqualFold(name[:len(p)], p) {
				return true
			}
		} else if strings.EqualFold(name, pat) {
			return true
		}
	}
	return false
}

func (b *Builder) headerIndexable(name string) bool {
	if matchHeaderList(name, b.opts.HeaderIncludes) {
		return true
	}
	return !matchHeaderList(name, b.opts.HeaderExcludes)
}

// buildState carries per-message state through the MIME walk: the UID being
// indexed, the remaining body-size budget, and (when DedupBodyParts is on)
// the set of already-seen normalized body-text hashes for THIS message only
// — cross-message dedup is out of scope, see Options.DedupBodyParts.
type buildState struct {
	uid        uint32
	remaining  int64
	seenHashes map[uint64]struct{}
}

// Build parses raw and streams the message's indexable parts into upd.
// The caller owns the update session (commit/rollback).
func (b *Builder) Build(uid uint32, raw io.Reader, upd fts.Update) error {
	e, err := message.Read(raw)
	if err != nil && !message.IsUnknownCharset(err) {
		return fmt.Errorf("fts/buildmail: parse: %w", err)
	}
	remaining := b.opts.MaxSize
	if remaining <= 0 {
		remaining = -1 // unlimited
	}
	st := &buildState{uid: uid, remaining: remaining}
	if b.opts.DedupBodyParts {
		st.seenHashes = make(map[uint64]struct{})
	}
	return b.walkEntity(st, e, 0, upd)
}

func (b *Builder) walkEntity(st *buildState, e *message.Entity, depth int, upd fts.Update) error {
	if err := b.buildHeaders(st.uid, e, depth, upd); err != nil {
		return err
	}

	mediaType, _, err := e.Header.ContentType()
	if err != nil || mediaType == "" {
		mediaType = "text/plain"
	}
	switch {
	case strings.HasPrefix(mediaType, "multipart/"):
		mr := e.MultipartReader()
		if mr == nil {
			return nil
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				// A broken part must not abort the whole message: index what
				// was readable.
				return nil
			}
			if err := b.walkEntity(st, part, depth+1, upd); err != nil {
				return err
			}
		}
	case strings.HasPrefix(mediaType, "message/"):
		nested, err := message.Read(e.Body)
		if err != nil && !message.IsUnknownCharset(err) {
			return nil
		}
		return b.walkEntity(st, nested, depth+1, upd)
	case strings.HasPrefix(mediaType, "text/"):
		return b.buildTextBody(st, e, mediaType, upd)
	default:
		return b.buildDecodedAttachment(st, e, mediaType, upd)
	}
}

func (b *Builder) buildHeaders(uid uint32, e *message.Entity, depth int, upd fts.Update) error {
	keyType := fts.KeyHeader
	if depth > 0 {
		keyType = fts.KeyMIMEHeader
	}
	fields := e.Header.Fields()
	for fields.Next() {
		name := fields.Key()
		if !b.headerIndexable(name) {
			continue
		}
		value, err := fields.Text()
		if err != nil {
			value = fields.Value() // undecodable encoded-word: index raw
		}
		if strings.TrimSpace(value) == "" {
			continue
		}
		accept, err := upd.SetBuildKey(fts.BuildKey{
			UID:     uid,
			Type:    keyType,
			HdrName: strings.ToLower(name),
		})
		if err != nil {
			return err
		}
		if !accept {
			continue
		}
		// A fresh session per field: tokenizer state must not leak between
		// fields (the reference resets the tokenizer per build key).
		session := b.chain.NewIndexSession(func(tok string) error {
			return upd.BuildMore([]byte(tok))
		})
		if err := session.Write([]byte(value)); err != nil {
			return err
		}
		if err := session.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (b *Builder) buildTextBody(st *buildState, e *message.Entity, mediaType string, upd fts.Update) error {
	produce := func(sink func([]byte) error) error {
		if mediaType == "text/html" {
			return htmlToText(e.Body, sink)
		}
		return copyChunks(e.Body, sink)
	}
	return b.buildBodyText(st, mediaType, produce, upd)
}

// buildDecodedAttachment routes a non-text/HTML part through the configured
// external decoder (nil Decoder = attachment stays unindexed, unchanged
// behaviour from before #669). A decoder returning ok=false (unsupported
// content type/extension) is treated the same as no decoder: skip silently.
func (b *Builder) buildDecodedAttachment(st *buildState, e *message.Entity, mediaType string, upd fts.Update) error {
	if b.opts.Decoder == nil {
		return nil
	}
	if st.remaining == 0 {
		return nil
	}
	_, dispParams, _ := e.Header.ContentDisposition()
	filename := dispParams["filename"]

	text, ok, err := b.opts.Decoder.Decode(context.Background(), mediaType, filename, e.Body)
	if err != nil || !ok || len(text) == 0 {
		// A decode failure must not abort the whole message: index what was
		// otherwise readable and move on, matching the tolerant-of-broken-
		// parts behaviour used throughout this walk.
		slog.Debug("fts/buildmail: decoder skipped attachment",
			"uid", st.uid, "content_type", mediaType, "filename", filename,
			"ok", ok, "err", err)
		return nil
	}
	slog.Debug("fts/buildmail: decoder extracted attachment text",
		"uid", st.uid, "content_type", mediaType, "filename", filename, "text_len", len(text))
	produce := func(sink func([]byte) error) error { return sink(text) }
	return b.buildBodyText(st, mediaType, produce, upd)
}

// buildBodyText is the shared tail for both text/HTML parts and decoded
// attachment text: applies the size cap, optionally dedups against this
// message's already-seen normalized text, and tokenizes into upd.
//
// DedupBodyParts is opt-in specifically because it changes the hot path:
// disabled (default), text streams directly into the tokenizer exactly as
// before #669 — zero extra buffering, zero extra allocation. Enabled, the
// part's text must be fully buffered first (bounded by MaxSize) so its
// normalized hash can be computed and compared before deciding to tokenize.
func (b *Builder) buildBodyText(st *buildState, contentType string, produce func(sink func([]byte) error) error, upd fts.Update) error {
	if st.remaining == 0 {
		return nil
	}

	capBytes := func(p []byte) []byte {
		if st.remaining > 0 && int64(len(p)) > st.remaining {
			p = p[:st.remaining]
		}
		if st.remaining > 0 {
			st.remaining -= int64(len(p))
		}
		return p
	}

	if b.opts.DedupBodyParts {
		// Buffer and hash BEFORE calling SetBuildKey: a duplicate must never
		// declare a key at all, only decide-and-discard after reading the
		// content. The read is bounded by st.remaining as an upper bound (via
		// a local copy, not yet deducted) purely to cap memory use — the
		// budget itself is only actually spent once we commit to indexing
		// below, so a duplicate that gets skipped costs nothing from it.
		var buf bytes.Buffer
		limit := st.remaining
		err := produce(func(p []byte) error {
			if limit == 0 {
				return errBodyCap
			}
			if limit > 0 && int64(len(p)) > limit {
				p = p[:limit]
			}
			if limit > 0 {
				limit -= int64(len(p))
			}
			buf.Write(p)
			return nil
		})
		if err != nil && err != errBodyCap {
			return nil // tolerate body read errors mid-part
		}
		text := buf.Bytes()
		h := normalizedTextHash(text)
		if _, dup := st.seenHashes[h]; dup {
			slog.Debug("fts/buildmail: dedup skipped duplicate body part",
				"uid", st.uid, "content_type", contentType, "text_len", len(text))
			return nil // duplicate content already indexed for this message
		}
		st.seenHashes[h] = struct{}{}
		slog.Debug("fts/buildmail: dedup indexing new body part",
			"uid", st.uid, "content_type", contentType, "text_len", len(text))

		accept, err := upd.SetBuildKey(fts.BuildKey{
			UID:         st.uid,
			Type:        fts.KeyBodyPart,
			ContentType: contentType,
		})
		if err != nil {
			return err
		}
		if !accept {
			return nil
		}
		text = capBytes(text)
		session := b.chain.NewIndexSession(func(tok string) error {
			return upd.BuildMore([]byte(tok))
		})
		if werr := session.Write(text); werr != nil {
			_ = session.Close()
			return werr
		}
		return session.Close()
	}

	accept, err := upd.SetBuildKey(fts.BuildKey{
		UID:         st.uid,
		Type:        fts.KeyBodyPart,
		ContentType: contentType,
	})
	if err != nil {
		return err
	}
	if !accept {
		return nil
	}
	session := b.chain.NewIndexSession(func(tok string) error {
		return upd.BuildMore([]byte(tok))
	})
	sinkErr := produce(func(p []byte) error {
		if st.remaining == 0 {
			return errBodyCap
		}
		return session.Write(capBytes(p))
	})
	if sinkErr != nil && sinkErr != errBodyCap {
		_ = session.Close()
		return nil
	}
	return session.Close()
}

var errBodyCap = fmt.Errorf("fts/buildmail: body size cap reached")

func copyChunks(r io.Reader, sink func([]byte) error) error {
	buf := make([]byte, 8192)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if serr := sink(buf[:n]); serr != nil {
				return serr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
