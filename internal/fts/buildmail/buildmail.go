// Package buildmail turns a raw RFC 5322 message into the FTS build-key
// stream: KeyHeader/KeyMIMEHeader for indexable header fields, KeyBodyPart for
// decoded text parts (HTML converted to text, other attachments via an
// optional external decoder). Skips multipart containers; caps indexed body
// size.
package buildmail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/mail"
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
	// (case-insensitive; trailing '*' is a prefix mask). Default is
	// index-unless-excluded; an include match wins over an exclude match.
	HeaderIncludes []string
	HeaderExcludes []string
	// MaxSize caps total body bytes fed to the index per message
	// (fts_message_max_size). 0 = unlimited.
	MaxSize int64

	// Decoder extracts text from non-text/HTML attachment parts (PDF, office
	// documents, etc.). Nil = such attachments stay unindexed
	// (fts_decoder_driver=none, the default).
	Decoder decoder.Decoder

	// DedupBodyParts skips re-tokenizing a body part whose normalized text was
	// already indexed for the SAME message (multipart/alternative text+html
	// twins, a repeated quoted block). Opt-in (fts_dedup_body_parts, default
	// false): the extra per-part hashing and buffering is off on the fast path.
	DedupBodyParts bool

	// DetectionSampleBytes bounds how many bytes of a part are read up front
	// for its language-detection sample (fts_detection_sample_bytes). 0 =
	// defaultDetectionSampleBytes. Only matters with more than one language.
	DetectionSampleBytes int
}

// Builder streams messages into an fts.Update. The language chain is selected
// per body/attachment part: exactly one auto-detected language per part (see
// MultiChain). Headers are not language text and always go through dataChain.
type Builder struct {
	opts      Options
	chain     *language.MultiChain
	dataChain *language.Chain
}

// New returns a Builder. chain is required.
func New(opts Options, chain *language.MultiChain) *Builder {
	// dataChain indexes header values (addresses, message-ids, subjects) with
	// normalization only: no stemming, no stopwords, regardless of the
	// configured language filters. internal/imap's header query expansion must
	// use the exact same chain, or a stemmed query variant against an
	// unstemmed indexed token yields false-positive wildcard matches.
	dataChain, err := language.NewDataChain()
	if err != nil {
		// "lowercase" is a static, language-independent filter: cannot fail at
		// runtime, only if the filter chain is broken at compile time.
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

// buildState carries per-message MIME-walk state: the UID, the remaining
// body-size budget, and (when DedupBodyParts is on) the already-seen
// normalized body-text hashes for THIS message only. No message-wide language
// chain: each part picks its own (see detectPartChain / selectChainForBytes).
type buildState struct {
	uid        uint32
	remaining  int64
	seenHashes map[uint64]struct{}
}

// defaultDetectionSampleBytes bounds the language-detection sample read from a
// part when Options.DetectionSampleBytes isn't set.
const defaultDetectionSampleBytes = 1024

// detectionRetryFactor grows the sample once (to
// detectSampleBytes()*detectionRetryFactor) when the first bounded prefix was
// too short/ambiguous to classify but more of the part is available. A single
// bounded growth step, not an open-ended retry loop.
const detectionRetryFactor = 4

// Build parses raw and streams the message's indexable parts into upd. The
// caller owns the update session (commit/rollback). Each body/attachment part
// detects its own language lazily, from its own text (see detectPartChain);
// headers always use dataChain.
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
// part shorter than n bytes just becomes the whole detection sample). r
// advances past what was consumed; the caller reattaches the remainder via
// io.MultiReader.
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

// extractSample turns a raw prefix into detector-ready text: as-is for plain
// text, tag-stripped for HTML (htmlToText tolerates truncated input).
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
// reading only a bounded prefix of r. It returns the chosen chain and a reader
// that replays the consumed prefix before the rest of r, so the indexing pass
// still sees the part's full content. With one language configured, detection
// is skipped and r is returned unread.
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

	// First prefix wasn't enough to classify. Retry with a larger sample only
	// if there's more to read (a short prefix already hit EOF).
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

// selectChainForBytes is detectPartChain's counterpart for content already
// fully in memory (decoded attachment text): detect on a bounded slice, with
// the same retry-once-if-more-is-available shape.
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
				// A broken part must not abort the message: index what was
				// readable.
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

// addressHeaders are the fields whose value is an RFC 5322 address-list; they
// get structured parsing before tokenization instead of tokenizing the raw
// decoded text as one blob.
var addressHeaders = map[string]bool{
	"from": true, "to": true, "cc": true, "bcc": true,
	"reply-to": true, "sender": true,
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
		raw := fields.Value()
		value, err := fields.Text()
		if err != nil {
			value = raw // undecodable encoded-word: index raw
		}
		if strings.TrimSpace(value) == "" {
			continue
		}

		// The header NAME itself gets its own build key with an empty HdrName:
		// it lands only in the A-pool (TEXT matches by header name, e.g.
		// "list-id"), never in the per-field H<NAME> pool alongside the value,
		// or HEADER List-Id "list-id" would match its own name.
		if accept, err := upd.SetBuildKey(fts.BuildKey{UID: st.uid, Type: keyType}); err != nil {
			return err
		} else if accept {
			if err := b.writeDataChain(name, upd); err != nil {
				return err
			}
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
		tokenizeText := value
		if addressHeaders[strings.ToLower(name)] {
			tokenizeText = addressHeaderText(raw, value)
		}
		if err := b.writeDataChain(tokenizeText, upd); err != nil {
			return err
		}
	}
	return nil
}

// writeDataChain runs text through a fresh no-stemming data-chain session into
// upd. Fresh per call: tokenizer state must not leak between build keys.
func (b *Builder) writeDataChain(text string, upd fts.Update) error {
	session := b.dataChain.NewIndexSession(func(tok string) error {
		return upd.BuildMore([]byte(tok))
	})
	if err := session.Write([]byte(text)); err != nil {
		return err
	}
	return session.Close()
}

// addressHeaderText re-derives an address-header's tokenizable text via
// structured RFC 5322 address-list parsing on the RAW (not RFC2047-decoded)
// bytes. Decoding encoded-words before parsing can turn decoded display-name
// characters ('(', '[', '<') into RFC 5322 comment/special delimiters and
// corrupt the parse; net/mail.ParseAddressList decodes as part of parsing the
// raw bytes, so parse-then-decode happens in one call. A parse failure falls
// back to the already-decoded value.
func addressHeaderText(raw, decoded string) string {
	addrs, err := mail.ParseAddressList(raw)
	if err != nil || len(addrs) == 0 {
		return decoded
	}
	var buf strings.Builder
	for _, a := range addrs {
		buf.WriteString(a.Name)
		buf.WriteByte(' ')
		buf.WriteString(a.Address)
		buf.WriteByte(' ')
	}
	return buf.String()
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
// external decoder (nil Decoder = attachment stays unindexed). A decoder
// returning ok=false (unsupported type/extension) is skipped silently.
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
	if err != nil {
		if errors.Is(err, decoder.ErrDegraded) {
			// Retries against a transient condition (network error, 5xx) were
			// exhausted: index this message without the attachment text and
			// move on. No second autoindex pass follows, since nothing about
			// the document is expected to change beyond the same failure.
			metricDecoderDegraded.Inc()
			slog.Info("fts/buildmail: decoder degraded, indexing without attachment text",
				"uid", st.uid, "content_type", mediaType, "filename", filename, "err", err)
			return nil
		}
		// Any other decoder error is a hard failure (bad config, a permanent
		// 4xx, a script protocol error): abort this message so autoindex
		// retries it later, rather than commit a silently-incomplete document.
		return fmt.Errorf("fts/buildmail: decode attachment: %w", err)
	}
	if !ok || len(text) == 0 {
		// Unsupported content type/extension: index what else was readable and
		// move on, like the rest of this walk tolerates broken parts.
		slog.Debug("fts/buildmail: decoder skipped attachment",
			"uid", st.uid, "content_type", mediaType, "filename", filename)
		return nil
	}
	slog.Debug("fts/buildmail: decoder extracted attachment text",
		"uid", st.uid, "content_type", mediaType, "filename", filename, "text_len", len(text))
	chain := b.selectChainForBytes(text)
	produce := func(sink func([]byte) error) error { return sink(text) }
	return b.buildBodyText(st, chain, mediaType, produce, upd)
}

// buildBodyText is the shared tail for text/HTML parts and decoded attachment
// text: applies the size cap, optionally dedups against this message's
// already-seen normalized text, and tokenizes into upd.
//
// DedupBodyParts is opt-in because it changes the hot path: disabled (default),
// text streams directly into the tokenizer with no extra buffering; enabled,
// the part's text is fully buffered first (bounded by MaxSize) so its
// normalized hash can be compared before deciding to tokenize.
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
		// Buffer and hash BEFORE SetBuildKey: a duplicate must never declare a
		// key, only decide-and-discard after reading the content. The read is
		// bounded by a local copy of st.remaining (not yet deducted) purely to
		// cap memory; the budget is spent only once indexing commits below, so
		// a skipped duplicate costs nothing from it.
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
		// A genuine mid-part read error (not just the size cap) is tolerated
		// like the non-dedup branch below: whatever was collected before the
		// error still gets hashed and indexed. Both branches must treat a
		// mid-read failure identically, or the same message indexes different
		// content depending on whether fts_dedup_body_parts is on.
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
