// Package buildmail turns a raw RFC 5322 message into the FTS build-key
// stream — the analogue of Dovecot's fts-build-mail.c. It walks the MIME
// structure, emits KeyHeader / KeyMIMEHeader for indexable header fields and
// KeyBodyPart for decoded text parts (HTML converted to text), skips
// multipart containers and binary parts, and caps the indexed body size.
package buildmail

import (
	"fmt"
	"io"
	"strings"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset"

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
	return b.walkEntity(uid, e, 0, &remaining, upd)
}

func (b *Builder) walkEntity(uid uint32, e *message.Entity, depth int, remaining *int64, upd fts.Update) error {
	if err := b.buildHeaders(uid, e, depth, upd); err != nil {
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
				// was readable (fts_build_mail tolerates parser errors).
				return nil
			}
			if err := b.walkEntity(uid, part, depth+1, remaining, upd); err != nil {
				return err
			}
		}
	case strings.HasPrefix(mediaType, "message/"):
		nested, err := message.Read(e.Body)
		if err != nil && !message.IsUnknownCharset(err) {
			return nil
		}
		return b.walkEntity(uid, nested, depth+1, remaining, upd)
	case strings.HasPrefix(mediaType, "text/"):
		return b.buildTextBody(uid, e, mediaType, remaining, upd)
	default:
		// Binary parts are skipped until the decoder phase (KeyBodyPartBinary
		// is only for engines that opt into raw binary).
		return nil
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

func (b *Builder) buildTextBody(uid uint32, e *message.Entity, mediaType string, remaining *int64, upd fts.Update) error {
	if *remaining == 0 {
		return nil
	}
	accept, err := upd.SetBuildKey(fts.BuildKey{
		UID:         uid,
		Type:        fts.KeyBodyPart,
		ContentType: mediaType,
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
	sink := func(p []byte) error {
		if *remaining == 0 {
			return errBodyCap
		}
		if *remaining > 0 && int64(len(p)) > *remaining {
			p = p[:*remaining]
		}
		if *remaining > 0 {
			*remaining -= int64(len(p))
		}
		return session.Write(p)
	}

	if mediaType == "text/html" {
		err = htmlToText(e.Body, sink)
	} else {
		err = copyChunks(e.Body, sink)
	}
	if err != nil && err != errBodyCap {
		// Tolerate body read errors mid-part: keep what was indexed.
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
