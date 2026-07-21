// Package decoder extracts indexable text from non-text/HTML MIME parts
// (PDF, office documents, etc.) via a pluggable external mechanism — no
// built-in format parsing, matching the reference implementation's own fts
// design (it has no built-in PDF/Word decoder either; it delegates to an
// external script or Apache Tika). See #669.
package decoder

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/config"
)

// Decoder extracts text from an attachment part. Implementations must be
// safe for concurrent use.
type Decoder interface {
	// Decode returns the extracted text for an attachment identified by
	// contentType and filename (filename may be empty). ok=false means this
	// decoder does not support the given type — the caller treats the part
	// as if no decoder were configured (skip it), not as an error.
	Decode(ctx context.Context, contentType, filename string, body io.Reader) (text []byte, ok bool, err error)
}

// New constructs the configured decoder. Returns (nil, nil) for driver
// "none" or empty — callers must treat a nil Decoder as "no attachment
// decoding", not call Decode on it.
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
		return newTikaDecoder(cfg.DecoderTikaURL, timeout, cfg.DecoderMaxSize), nil
	default:
		return nil, fmt.Errorf("fts/decoder: unknown fts_decoder_driver %q", cfg.DecoderDriver)
	}
}
