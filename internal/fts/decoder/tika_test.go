package decoder

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTikaDecoderSuccess(t *testing.T) {
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		if r.Method != http.MethodPut || r.URL.Path != "/tika" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "pdf bytes" {
			t.Errorf("body = %q, want %q", body, "pdf bytes")
		}
		_, _ = w.Write([]byte("extracted by tika"))
	}))
	defer srv.Close()

	d := newTikaDecoder(srv.URL, 5*time.Second, 0, 1)
	text, ok, err := d.Decode(context.Background(), "application/pdf", "report.pdf", strings.NewReader("pdf bytes"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !ok || string(text) != "extracted by tika" {
		t.Fatalf("text=%q ok=%v", text, ok)
	}
	if gotContentType != "application/pdf" {
		t.Fatalf("Content-Type sent = %q, want application/pdf", gotContentType)
	}
}

func TestTikaDecoderFilenameSentAsContentDisposition(t *testing.T) {
	var gotDisposition string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotDisposition = r.Header.Get("Content-Disposition")
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	d := newTikaDecoder(srv.URL, 5*time.Second, 0, 1)
	_, _, err := d.Decode(context.Background(), "application/octet-stream", "invoice 2024.pdf", strings.NewReader("bytes"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !strings.Contains(gotDisposition, `filename="invoice 2024.pdf"`) {
		t.Fatalf("Content-Disposition = %q, want it to carry the filename", gotDisposition)
	}
}

func TestTikaDecoderUnsupportedMediaType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnsupportedMediaType)
	}))
	defer srv.Close()

	d := newTikaDecoder(srv.URL, 5*time.Second, 0, 1)
	_, ok, err := d.Decode(context.Background(), "application/x-bogus", "", strings.NewReader("bytes"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for 415 response")
	}
}

func TestTikaDecoderNoContentIsSkip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	d := newTikaDecoder(srv.URL, 5*time.Second, 0, 1)
	_, ok, err := d.Decode(context.Background(), "application/pdf", "", strings.NewReader("bytes"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for 204 response")
	}
}

func TestTikaDecoderEmptyBodyIsSkip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTikaDecoder(srv.URL, 5*time.Second, 0, 1)
	_, ok, err := d.Decode(context.Background(), "application/pdf", "", strings.NewReader("bytes"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for empty extracted text")
	}
}

// TestTikaDecoderServerErrorExhaustsRetriesAndDegrades (#697): a persistent
// 5xx is retried up to maxAttempts times, then returns an error wrapping
// ErrDegraded — the caller indexes the message without this attachment's
// text instead of failing it outright.
func TestTikaDecoderServerErrorExhaustsRetriesAndDegrades(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := newTikaDecoder(srv.URL, 5*time.Second, 0, 3)
	_, ok, err := d.Decode(context.Background(), "application/pdf", "", strings.NewReader("bytes"))
	if err == nil {
		t.Fatal("expected an error after exhausting retries")
	}
	if !errors.Is(err, ErrDegraded) {
		t.Fatalf("err = %v, want it to wrap ErrDegraded", err)
	}
	if ok {
		t.Fatal("expected ok=false alongside the error")
	}
	if n := requests.Load(); n != 3 {
		t.Fatalf("server received %d requests, want exactly maxAttempts=3", n)
	}
}

// TestTikaDecoderTransientErrorRecoversOnRetry proves a transient 5xx that
// clears up on the second attempt succeeds normally — retrying isn't just a
// slower way to fail, it actually recovers.
func TestTikaDecoderTransientErrorRecoversOnRetry(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("recovered"))
	}))
	defer srv.Close()

	d := newTikaDecoder(srv.URL, 5*time.Second, 0, 2)
	text, ok, err := d.Decode(context.Background(), "application/pdf", "", strings.NewReader("bytes"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !ok || string(text) != "recovered" {
		t.Fatalf("text=%q ok=%v, want recovery on the second attempt", text, ok)
	}
}

// TestTikaDecoderHardClientErrorNoRetry (#697): a non-retryable 4xx (bad
// request, auth, ...) is a hard, immediate failure — never retried, never
// classified as ErrDegraded, so buildmail aborts the message instead of
// silently indexing it without the attachment.
func TestTikaDecoderHardClientErrorNoRetry(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	d := newTikaDecoder(srv.URL, 5*time.Second, 0, 3)
	_, ok, err := d.Decode(context.Background(), "application/pdf", "", strings.NewReader("bytes"))
	if err == nil {
		t.Fatal("expected an error for 400 response")
	}
	if errors.Is(err, ErrDegraded) {
		t.Fatal("a hard 4xx must not be classified as ErrDegraded")
	}
	if ok {
		t.Fatal("expected ok=false alongside the error")
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("server received %d requests, want exactly 1 (no retry for a hard error)", n)
	}
}

func TestTikaDecoderMaxSizeTruncates(t *testing.T) {
	var gotLen int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotLen = len(body)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	d := newTikaDecoder(srv.URL, 5*time.Second, 4, 1)
	_, _, err := d.Decode(context.Background(), "application/pdf", "", strings.NewReader("this is way more than four bytes"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if gotLen != 4 {
		t.Fatalf("server received %d bytes, want capped to 4", gotLen)
	}
}
