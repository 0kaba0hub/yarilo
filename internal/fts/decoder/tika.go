package decoder

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
)

// defaultTikaMaxAttempts mirrors the reference implementation's own Tika
// plugin default: one initial attempt plus one retry (#697).
const defaultTikaMaxAttempts = 2

// tikaRetryBase is the exponential-backoff base delay between retries.
const tikaRetryBase = 200 * time.Millisecond

type tikaDecoder struct {
	baseURL     string
	client      *http.Client
	maxSize     int64
	maxAttempts int
}

func newTikaDecoder(baseURL string, timeout time.Duration, maxSize int64, maxAttempts int) *tikaDecoder {
	if maxAttempts <= 0 {
		maxAttempts = defaultTikaMaxAttempts
	}
	return &tikaDecoder{
		baseURL:     strings.TrimRight(baseURL, "/"),
		client:      &http.Client{Timeout: timeout},
		maxSize:     maxSize,
		maxAttempts: maxAttempts,
	}
}

// Decode PUTs the attachment to Tika's /tika endpoint and reads back the
// extracted plain text.
//
// 415/422/204 mean Tika understood the request but has nothing to extract
// (unsupported type / no content) — treated as "unsupported" (ok=false),
// not an error, same as an unconfigured decoder.
//
// A connection error or 5xx is retried (bounded, exponential backoff): a
// transient Tika restart must not permanently degrade the index the way
// silently skipping did before #697. Any other non-2xx status is a hard,
// non-retryable failure (bad request, auth, config) — returned immediately
// so the caller aborts this message's indexing attempt. Once retries are
// exhausted against a transient condition, the error wraps ErrDegraded so
// the caller can index the message without this attachment's text instead
// of failing it outright.
//
// The attachment is read fully into memory up front (bounded by maxSize)
// because a retry needs to resend the same bytes — an http.Request body
// reader can only be consumed once.
func (d *tikaDecoder) Decode(ctx context.Context, contentType, filename string, body io.Reader) ([]byte, bool, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, false, fmt.Errorf("fts/decoder/tika: read attachment: %w", err)
	}
	if d.maxSize > 0 && int64(len(data)) > d.maxSize {
		data = data[:d.maxSize]
	}

	var lastErr error
	for attempt := 0; attempt < d.maxAttempts; attempt++ {
		if attempt > 0 {
			delay := tikaRetryBase * time.Duration(int64(1)<<(attempt-1))
			select {
			case <-ctx.Done():
				return nil, false, ctx.Err()
			case <-time.After(delay):
			}
		}
		text, ok, retry, err := d.attempt(ctx, contentType, filename, data)
		if !retry {
			return text, ok, err
		}
		lastErr = err
	}
	return nil, false, fmt.Errorf("fts/decoder/tika: giving up after %d attempts: %w: %w", d.maxAttempts, ErrDegraded, lastErr)
}

// attempt runs a single PUT. retry=true means the caller should back off and
// try again (network error or 5xx); retry=false means the result — success,
// unsupported, or a hard error — is final.
func (d *tikaDecoder) attempt(ctx context.Context, contentType, filename string, data []byte) (text []byte, ok bool, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, d.baseURL+"/tika", bytes.NewReader(data))
	if err != nil {
		return nil, false, false, fmt.Errorf("fts/decoder/tika: build request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if filename != "" {
		req.Header.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	}
	req.Header.Set("Accept", "text/plain")

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, false, true, fmt.Errorf("fts/decoder/tika: request: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity, http.StatusNoContent:
		return nil, false, false, nil
	}
	if resp.StatusCode >= 500 {
		return nil, false, true, fmt.Errorf("fts/decoder/tika: server error %d", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, false, fmt.Errorf("fts/decoder/tika: unexpected status %d", resp.StatusCode)
	}
	text, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, true, fmt.Errorf("fts/decoder/tika: read response: %w", err)
	}
	if len(text) == 0 {
		return nil, false, false, nil
	}
	return text, true, false, nil
}
