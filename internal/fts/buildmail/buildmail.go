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
	"errors"
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

// Builder streams messages into an fts.Update through a language chain,
// selected per message (#668 point 3: exactly one auto-detected language at
// index time, never redundant per-language re-stemming — see MultiChain).
type Builder struct {
	opts  Options
	chain *language.MultiChain
}

// New returns a Builder. chain is required: the first engine is tokenized,
// so every part streams through the tokenizer + filters.
func New(opts Options, chain *language.MultiChain) *Builder {
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
// indexed, the remaining body-size budget, the language chain selected for
// THIS message (#668 point 3), and (when DedupBodyParts is on) the set of
// already-seen normalized body-text hashes for THIS message only —
// cross-message dedup is out of scope, see Options.DedupBodyParts.
type buildState struct {
	uid        uint32
	remaining  int64
	chain      *language.Chain
	seenHashes map[uint64]struct{}
}

// sampleMaxBytes bounds how much decoded text detectSample collects before
// stopping — enough for reliable language detection without buffering the
// whole message a second time.
const sampleMaxBytes = 1024

// detectionPrefixCap bounds how many raw bytes Build reads up front to
// derive a language-detection sample when multiple languages are
// configured (#695). The prefix is stitched back onto the rest of the
// stream via io.MultiReader before the real (single) parse pass, so
// indexing still sees the whole message — but at most this many bytes are
// ever held in memory at once, regardless of the message's actual size.
const detectionPrefixCap = 8192

// Build parses raw and streams the message's indexable parts into upd.
// The caller owns the update session (commit/rollback).
//
// Language selection (#668 point 3) needs a text sample BEFORE the real
// indexing walk starts, since headers are tokenized through the selected
// chain too — not just body parts. Rather than buffering the whole message
// to get that sample (#695 — a 500MB message must not be fully
// materialized just to pick a stemmer), only a bounded prefix
// (detectionPrefixCap) is read and sampled; a truncated/unparseable prefix
// simply yields an empty sample, which SelectForIndex already treats as
// "insufficient data" and falls back to the first configured language —
// the same outcome as genuinely short text. The prefix is then reattached
// to the remaining stream for a single real parse + indexing walk. The
// (much more common) single-language case skips even this bounded read —
// detection never runs at all, so there is nothing to sample.
func (b *Builder) Build(uid uint32, raw io.Reader, upd fts.Update) error {
	remaining := b.opts.MaxSize
	if remaining <= 0 {
		remaining = -1 // unlimited
	}

	var chain *language.Chain
	reader := raw
	if b.chain.NeedsDetection() {
		prefix, err := readPrefix(raw, detectionPrefixCap)
		if err != nil {
			return fmt.Errorf("fts/buildmail: read: %w", err)
		}
		chain, _ = b.chain.SelectForIndex(detectSample(prefix))
		reader = io.MultiReader(bytes.NewReader(prefix), raw)
	} else {
		chain, _ = b.chain.SelectForIndex("")
	}

	e, err := message.Read(reader)
	if err != nil && !message.IsUnknownCharset(err) {
		return fmt.Errorf("fts/buildmail: parse: %w", err)
	}

	st := &buildState{uid: uid, remaining: remaining, chain: chain}
	if b.opts.DedupBodyParts {
		st.seenHashes = make(map[uint64]struct{})
	}
	return b.walkEntity(st, e, 0, upd)
}

// readPrefix reads up to n bytes from r, tolerating a short read at EOF
// (a message shorter than n bytes is not an error — it just means the
// entire message became the detection sample). r's read position advances
// past whatever was consumed; the caller reattaches the remainder via
// io.MultiReader rather than "unreading" it.
func readPrefix(r io.Reader, n int) ([]byte, error) {
	buf := make([]byte, n)
	read, err := io.ReadFull(r, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:read], nil
}

// detectSample extracts a bounded, representative text excerpt for language
// detection: the Subject header plus the start of the first text body part
// encountered (text/plain preferred as-is; text/html tag-stripped). Parse
// errors are tolerated — an empty/partial sample just means detectLanguage
// falls back to the first configured language, the same outcome as
// genuinely insufficient text.
func detectSample(rawBytes []byte) string {
	e, err := message.Read(bytes.NewReader(rawBytes))
	if err != nil && !message.IsUnknownCharset(err) {
		return ""
	}
	var buf bytes.Buffer
	if subj, err := e.Header.Text("Subject"); err == nil {
		buf.WriteString(subj)
		buf.WriteByte(' ')
	}
	sampleTextBody(e, &buf)
	return buf.String()
}

// sampleTextBody walks the MIME tree collecting decoded text into buf, up
// to sampleMaxBytes, stopping as soon as the cap is reached.
func sampleTextBody(e *message.Entity, buf *bytes.Buffer) {
	if buf.Len() >= sampleMaxBytes {
		return
	}
	mediaType, _, err := e.Header.ContentType()
	if err != nil || mediaType == "" {
		mediaType = "text/plain"
	}
	switch {
	case strings.HasPrefix(mediaType, "multipart/"):
		mr := e.MultipartReader()
		if mr == nil {
			return
		}
		for buf.Len() < sampleMaxBytes {
			part, err := mr.NextPart()
			if err != nil {
				return
			}
			sampleTextBody(part, buf)
		}
	case strings.HasPrefix(mediaType, "message/"):
		nested, err := message.Read(e.Body)
		if err != nil && !message.IsUnknownCharset(err) {
			return
		}
		sampleTextBody(nested, buf)
	case strings.HasPrefix(mediaType, "text/"):
		sink := func(p []byte) error {
			remaining := sampleMaxBytes - buf.Len()
			if remaining <= 0 {
				return errBodyCap
			}
			if len(p) > remaining {
				p = p[:remaining]
			}
			buf.Write(p)
			return nil
		}
		if mediaType == "text/html" {
			_ = htmlToText(e.Body, sink)
		} else {
			_ = copyChunks(e.Body, sink)
		}
	}
}

func (b *Builder) walkEntity(st *buildState, e *message.Entity, depth int, upd fts.Update) error {
	if err := b.buildHeaders(st, e, depth, upd); err != nil {
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

func (b *Builder) buildHeaders(st *buildState, e *message.Entity, depth int, upd fts.Update) error {
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
			UID:     st.uid,
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
		session := st.chain.NewIndexSession(func(tok string) error {
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
		_ = produce(func(p []byte) error {
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
		// A genuine read error mid-part (not just hitting the size cap) is
		// tolerated the same way the non-dedup branch below tolerates one:
		// whatever text was collected BEFORE the error still gets hashed and
		// indexed, rather than discarding the whole part. The two branches
		// must treat a mid-read failure identically — otherwise the same
		// message indexes different content purely based on whether
		// fts_dedup_body_parts is on.
		text := buf.Bytes()
		if len(text) == 0 {
			return nil // nothing readable at all
		}
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
		session := st.chain.NewIndexSession(func(tok string) error {
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
	session := st.chain.NewIndexSession(func(tok string) error {
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
