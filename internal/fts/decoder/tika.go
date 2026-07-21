package decoder

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type tikaDecoder struct {
	baseURL string
	client  *http.Client
	maxSize int64
}

func newTikaDecoder(baseURL string, timeout time.Duration, maxSize int64) *tikaDecoder {
	return &tikaDecoder{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: timeout},
		maxSize: maxSize,
	}
}

// Decode PUTs the attachment to Tika's /tika endpoint and reads back the
// extracted plain text. A non-2xx response (Tika could not parse this
// content type) is treated as "unsupported" (ok=false), not an error — the
// caller falls back to skipping the part, same as an unconfigured decoder.
func (d *tikaDecoder) Decode(ctx context.Context, contentType, filename string, body io.Reader) ([]byte, bool, error) {
	r := body
	if d.maxSize > 0 {
		r = io.LimitReader(body, d.maxSize)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, d.baseURL+"/tika", r)
	if err != nil {
		return nil, false, fmt.Errorf("fts/decoder/tika: build request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "text/plain")

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("fts/decoder/tika: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnsupportedMediaType || resp.StatusCode == http.StatusUnprocessableEntity {
		return nil, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("fts/decoder/tika: unexpected status %d", resp.StatusCode)
	}
	text, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, fmt.Errorf("fts/decoder/tika: read response: %w", err)
	}
	if len(text) == 0 {
		return nil, false, nil
	}
	return text, true, nil
}
