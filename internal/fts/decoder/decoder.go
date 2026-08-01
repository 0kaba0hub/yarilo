// Package decoder extracts indexable text from non-text MIME parts (PDF,
// office documents, etc.) via an external script or Apache Tika; no
// built-in format parsing.
package decoder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/config"
)

// Decoder extracts text from an attachment part. Implementations must be
// safe for concurrent use.
type Decoder interface {
	// Decode returns the extracted text for an attachment; filename may be
	// empty. ok=false means unsupported type: skip the part, not an error.
	// err wrapping ErrDegraded means retries exhausted on a transient
	// condition: index the message without this attachment's text. Any
	// other err is a hard failure and must abort this message's indexing
	// so it gets retried later, not committed incomplete.
	Decode(ctx context.Context, contentType, filename string, body io.Reader) (text []byte, ok bool, err error)
}

// ErrDegraded marks a Decode error as "index without this attachment's
// text", not "fail the message".
var ErrDegraded = errors.New("fts/decoder: degraded (retries exhausted)")

// New constructs the configured decoder. Returns (nil, nil) for driver
// "none" or empty; a nil Decoder means no attachment decoding.
func New(cfg config.FTSConfig) (Decoder, error) {
	timeout := time.Duration(cfg.DecoderTimeoutSecs) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	switch cfg.DecoderDriver {
	case "", "none":
		return nil, nil
	case "script":
		if cfg.DecoderScriptAddr == "" {
			return nil, fmt.Errorf("fts/decoder: fts_decoder_driver=script requires fts_decoder_script_addr")
		}
		return newScriptDecoder(cfg.DecoderScriptAddr, timeout, cfg.DecoderMaxSize), nil
	case "tika":
		if cfg.DecoderTikaURL == "" {
			return nil, fmt.Errorf("fts/decoder: fts_decoder_driver=tika requires fts_decoder_tika_url")
		}
		return newTikaDecoder(cfg.DecoderTikaURL, timeout, cfg.DecoderMaxSize, cfg.DecoderMaxAttempts), nil
	default:
		return nil, fmt.Errorf("fts/decoder: unknown fts_decoder_driver %q", cfg.DecoderDriver)
	}
}
