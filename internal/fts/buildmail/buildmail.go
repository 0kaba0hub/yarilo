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

	// DetectionSampleBytes bounds how many bytes of a body/attachment part
	// are read up front to derive its language-detection sample (#696;
	// fts_detection_sample_bytes). 0 = defaultDetectionSampleBytes. Only
	// matters when the configured chain has more than one language.
	DetectionSampleBytes int
}

// Builder streams messages into an fts.Update through a language chain,
// selected per body/attachment part (#668 point 3, refined by #696: exactly
// one auto-detected language per PART, never redundant per-language
// re-stemming — see MultiChain). Headers are not language text and always
// go through dataChain instead (#696 point 2).
type Builder struct {
	opts      Options
	chain     *language.MultiChain
	dataChain *language.Chain
}

// New returns a Builder. chain is required: the first engine is tokenized,
// so every part streams through the tokenizer + filters.
func New(opts Options, chain *language.MultiChain) *Builder {
	// dataChain indexes header values (addresses, message-ids, subjects in
	// arbitrary languages) with normalization only — no stemming, no
	// stopwords, regardless of the configured language filter set. Search
	// side already matches these tokens fine: ExpandSearch always keeps the
	// raw tokenized query term as one of a Word's OR variants (#696 point 2).
	dataChain, err := language.NewChain(language.Settings{Language: "data", Filters: []string{"lowercase"}})
	if err != nil {
		// "lowercase" is a static, language-independent filter — this
		// cannot fail in practice; a panic here would only mean the filter
		// chain itself was broken at compile time, not a runtime condition.
		panic(fmt.Sprintf("fts/buildmail: data chain: %v", err))
	}
	return &Builder{opts: opts, chain: chain, dataChain: dataChain}
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
// — cross-message dedup is out of scope, see Options.DedupBodyParts. Unlike
// before #696, there is no message-wide language chain here: each part
// picks its own (see detectPartChain / selectChainForBytes).
type buildState struct {
	uid        uint32
	remaining  int64
	seenHashes map[uint64]struct{}
}

// defaultDetectionSampleBytes bounds how many raw bytes of a body/attachment
// part are read up front to derive its language-detection sample, when
// Options.DetectionSampleBytes isn't set.
const defaultDetectionSampleBytes = 1024

// detectionRetryFactor grows the sample once (to
// detectSampleBytes()*detectionRetryFactor) when the first bounded prefix
// was too short/ambiguous to classify but more of the part is available to
// read — mirroring the reference implementation's retry-before-fallback
// behaviour (#696 point 4) with a single bounded growth step rather than an
// open-ended retry loop.
const detectionRetryFactor = 4

// Build parses raw and streams the message's indexable parts into upd. The
// caller owns the update session (commit/rollback).
//
// #696 removed the message-wide pre-pass that #695 had bounded: headers no
// longer go through a detected language chain at all (buildHeaders always
// uses dataChain), so there is nothing that needs a sample before the walk
// starts. Each body/attachment part now detects its own language lazily,
// from its own text, exactly when it is built — see detectPartChain.
func (b *Builder) Build(uid uint32, raw io.Reader, upd fts.Update) error {
	remaining := b.opts.MaxSize
	if remaining <= 0 {
		remaining = -1 // unlimited
	}

	e, err := message.Read(raw)
	if err != nil && !message.IsUnknownCharset(err) {
		return fmt.Errorf("fts/buildmail: parse: %w", err)
	}

	st := &buildState{uid: uid, remaining: remaining}
	if b.opts.DedupBodyParts {
		st.seenHashes = make(map[uint64]struct{})
	}
	return b.walkEntity(st, e, 0, upd)
}

// readPrefix reads up to n bytes from r, tolerating a short read at EOF (a
// part shorter than n bytes is not an error — it just means the entire part
// became the detection sample). r's read position advances past whatever
// was consumed; the caller reattaches the remainder via io.MultiReader
// rather than "unreading" it.
func readPrefix(r io.Reader, n int) ([]byte, error) {
	buf := make([]byte, n)
	read, err := io.ReadFull(r, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:read], nil
}

func (b *Builder) detectSampleBytes() int {
	if b.opts.DetectionSampleBytes > 0 {
		return b.opts.DetectionSampleBytes
	}
	return defaultDetectionSampleBytes
}

// extractSample turns a raw prefix into detector-ready text: as-is for
// plain text, tag-stripped for HTML (tolerant of a prefix that cuts a tag
// in half — htmlToText already tolerates malformed/truncated input, see
// html.go).
func extractSample(prefix []byte, isHTML bool) string {
	if !isHTML {
		return string(prefix)
	}
	var buf bytes.Buffer
	_ = htmlToText(bytes.NewReader(prefix), func(p []byte) error {
		buf.Write(p)
		return nil
	})
	return buf.String()
}

// detectPartChain selects the language chain for one body part's own text,
// reading only a bounded prefix of r (#696: per-part detection, generalizing
// #695's message-level bounded-prefix pattern down to the part level). It
// returns the chosen chain and a reader that replays the consumed prefix
// before continuing with whatever remains of r, so the real indexing pass
// still sees the part's full content.
//
// When only one language is configured, detection never runs at all — r is
// returned unread, matching the zero-overhead single-language path #695
// established at the message level.
func (b *Builder) detectPartChain(r io.Reader, isHTML bool) (*language.Chain, io.Reader, error) {
	if !b.chain.NeedsDetection() {
		chain, _ := b.chain.SelectForIndex("")
		return chain, r, nil
	}

	cap0 := b.detectSampleBytes()
	prefix, err := readPrefix(r, cap0)
	if err != nil {
		return nil, nil, fmt.Errorf("fts/buildmail: read: %w", err)
	}

	if chain, _, ok := b.chain.TryDetect(extractSample(prefix, isHTML)); ok {
		return chain, io.MultiReader(bytes.NewReader(prefix), r), nil
	}

	// The first bounded prefix wasn't enough to classify reliably. Retry
	// with a larger sample only if there's actually more to read (a short
	// prefix already hit EOF, so growing it would read nothing new).
	if len(prefix) == cap0 {
		extra, err := readPrefix(r, cap0*detectionRetryFactor-cap0)
		if err != nil {
			return nil, nil, fmt.Errorf("fts/buildmail: read: %w", err)
		}
		prefix = append(prefix, extra...)
		if chain, _, ok := b.chain.TryDetect(extractSample(prefix, isHTML)); ok {
			return chain, io.MultiReader(bytes.NewReader(prefix), r), nil
		}
	}

	chain, _ := b.chain.SelectForIndex("") // per-part fallback: first configured language
	return chain, io.MultiReader(bytes.NewReader(prefix), r), nil
}

// selectChainForBytes is detectPartChain's counterpart for content that is
// already fully materialized in memory (decoded attachment text, #696 point
// 3) — no bounded-read/replay dance needed, just detect on a bounded slice
// of what's already there, with the same retry-once-if-more-is-available
// shape as detectPartChain.
func (b *Builder) selectChainForBytes(data []byte) *language.Chain {
	if !b.chain.NeedsDetection() {
		chain, _ := b.chain.SelectForIndex("")
		return chain
	}

	cap0 := b.detectSampleBytes()
	sample := data
	if len(sample) > cap0 {
		sample = sample[:cap0]
	}
	if chain, _, ok := b.chain.TryDetect(string(sample)); ok {
		return chain
	}

	cap1 := cap0 * detectionRetryFactor
	if len(data) > cap0 && len(data) > len(sample) {
		sample2 := data
		if len(sample2) > cap1 {
			sample2 = sample2[:cap1]
		}
		if chain, _, ok := b.chain.TryDetect(string(sample2)); ok {
			return chain
		}
	}

	chain, _ := b.chain.SelectForIndex("") // per-part fallback: first configured language
	return chain
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
		// fields (the reference resets the tokenizer per build key). Headers
		// are not language text (#696 point 2) — always dataChain, never a
		// detected language, regardless of which part surrounds them.
		session := b.dataChain.NewIndexSession(func(tok string) error {
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
	isHTML := mediaType == "text/html"
	chain, reader, err := b.detectPartChain(e.Body, isHTML)
	if err != nil {
		return err
	}
	produce := func(sink func([]byte) error) error {
		if isHTML {
			return htmlToText(reader, sink)
		}
		return copyChunks(reader, sink)
	}
	return b.buildBodyText(st, chain, mediaType, produce, upd)
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
	chain := b.selectChainForBytes(text)
	produce := func(sink func([]byte) error) error { return sink(text) }
	return b.buildBodyText(st, chain, mediaType, produce, upd)
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
func (b *Builder) buildBodyText(st *buildState, chain *language.Chain, contentType string, produce func(sink func([]byte) error) error, upd fts.Update) error {
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
		session := chain.NewIndexSession(func(tok string) error {
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
	session := chain.NewIndexSession(func(tok string) error {
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
