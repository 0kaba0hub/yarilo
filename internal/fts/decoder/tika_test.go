package decoder

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

	d := newTikaDecoder(srv.URL, 5*time.Second, 0)
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

func TestTikaDecoderUnsupportedMediaType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnsupportedMediaType)
	}))
	defer srv.Close()

	d := newTikaDecoder(srv.URL, 5*time.Second, 0)
	_, ok, err := d.Decode(context.Background(), "application/x-bogus", "", strings.NewReader("bytes"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for 415 response")
	}
}

func TestTikaDecoderEmptyBodyIsSkip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTikaDecoder(srv.URL, 5*time.Second, 0)
	_, ok, err := d.Decode(context.Background(), "application/pdf", "", strings.NewReader("bytes"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for empty extracted text")
	}
}

func TestTikaDecoderServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := newTikaDecoder(srv.URL, 5*time.Second, 0)
	_, ok, err := d.Decode(context.Background(), "application/pdf", "", strings.NewReader("bytes"))
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if ok {
		t.Fatal("expected ok=false alongside the error")
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

	d := newTikaDecoder(srv.URL, 5*time.Second, 4)
	_, _, err := d.Decode(context.Background(), "application/pdf", "", strings.NewReader("this is way more than four bytes"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if gotLen != 4 {
		t.Fatalf("server received %d bytes, want capped to 4", gotLen)
	}
}
