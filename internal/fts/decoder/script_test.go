package decoder

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// shortSocketPath returns a Unix-socket path short enough to satisfy
// sockaddr_un.sun_path on macOS/BSD (104 bytes) — t.TempDir() under
// /var/folders on macOS is too long once combined with a descriptive test
// name.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "yl")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "d.sock")
}

func readFull(r *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// scriptServer is a minimal test double for the wire protocol, handling the
// mandatory VERSION handshake plus either a TYPES query or a DECODE request
// per connection (#697's TYPES prefilter is fetched on its own dedicated
// connection, separate from any DECODE connection — a real script decoder,
// and this double, must handle both).
type scriptServer struct {
	// typesMode controls the TYPES response: "list" sends types (possibly
	// empty), "error" sends an ERROR line, "hang" sleeps past the caller's
	// deadline before responding, "close" closes the connection immediately
	// without responding (an old v1 script that never recognized TYPES at
	// all wouldn't reply either way). Empty defaults to "close".
	typesMode  string
	types      []string
	hangFor    time.Duration
	typesCalls atomic.Int32

	decode      func(contentType, filename string, body []byte) (respFields []string, extraText []byte)
	decodeCalls atomic.Int32
}

func (s *scriptServer) listen(t *testing.T, network, addr string) net.Listener {
	t.Helper()
	ln, err := net.Listen(network, addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(conn)
		}
	}()
	return ln
}

func (s *scriptServer) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	if _, err := readFields(r); err != nil { // VERSION request
		return
	}
	_ = writeLine(conn, scriptCmdVersion, scriptProtocolVersion, scriptRespOK)

	fields, err := readFields(r)
	if err != nil {
		return
	}
	switch fields[0] {
	case scriptCmdTypes:
		s.typesCalls.Add(1)
		switch s.typesMode {
		case "error":
			_ = writeLine(conn, scriptRespError, "types not supported")
		case "hang":
			time.Sleep(s.hangFor)
		case "list":
			for _, line := range s.types {
				_, _ = io.WriteString(conn, line+"\n")
			}
			_, _ = io.WriteString(conn, "\n")
		default: // "close" / unset: an old v1 script that doesn't know TYPES
			return
		}
	case scriptCmdDecode:
		s.decodeCalls.Add(1)
		if len(fields) != 4 {
			return
		}
		n, _ := strconv.Atoi(fields[3])
		body := make([]byte, n)
		_, _ = readFull(r, body)
		if s.decode == nil {
			_ = writeLine(conn, scriptRespSkip)
			return
		}
		respFields, extra := s.decode(fields[1], fields[2], body)
		_ = writeLine(conn, respFields...)
		if extra != nil {
			_, _ = conn.Write(extra)
		}
	}
}

func okDecodeFn(text string) func(string, string, []byte) ([]string, []byte) {
	return func(string, string, []byte) ([]string, []byte) {
		return []string{scriptRespOK, strconv.Itoa(len(text))}, []byte(text)
	}
}

func TestScriptDecoderSuccess(t *testing.T) {
	sock := shortSocketPath(t)
	srv := &scriptServer{decode: okDecodeFn("extracted text here")}
	ln := srv.listen(t, "unix", sock)
	defer ln.Close()

	d := newScriptDecoder("unix://"+sock, 5*time.Second, 0)
	text, ok, err := d.Decode(context.Background(), "application/pdf", "report.pdf", strings.NewReader("%PDF-1.4 fake bytes"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if string(text) != "extracted text here" {
		t.Fatalf("text = %q, want %q", text, "extracted text here")
	}
}

func TestScriptDecoderSkip(t *testing.T) {
	sock := shortSocketPath(t)
	srv := &scriptServer{decode: func(string, string, []byte) ([]string, []byte) {
		return []string{scriptRespSkip}, nil
	}}
	ln := srv.listen(t, "unix", sock)
	defer ln.Close()

	d := newScriptDecoder("unix://"+sock, 5*time.Second, 0)
	_, ok, err := d.Decode(context.Background(), "application/zip", "", strings.NewReader("zip bytes"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for SKIP response")
	}
}

func TestScriptDecoderError(t *testing.T) {
	sock := shortSocketPath(t)
	srv := &scriptServer{decode: func(string, string, []byte) ([]string, []byte) {
		return []string{scriptRespError, "decoder crashed"}, nil
	}}
	ln := srv.listen(t, "unix", sock)
	defer ln.Close()

	d := newScriptDecoder("unix://"+sock, 5*time.Second, 0)
	_, ok, err := d.Decode(context.Background(), "application/pdf", "", strings.NewReader("bytes"))
	if err == nil {
		t.Fatal("expected error for ERROR response")
	}
	if ok {
		t.Fatal("expected ok=false alongside the error")
	}
}

func TestScriptDecoderMaxSizeTruncates(t *testing.T) {
	sock := shortSocketPath(t)
	var gotSize int
	srv := &scriptServer{decode: func(_, _ string, body []byte) ([]string, []byte) {
		gotSize = len(body)
		return []string{scriptRespOK, "0"}, nil
	}}
	ln := srv.listen(t, "unix", sock)
	defer ln.Close()

	d := newScriptDecoder("unix://"+sock, 5*time.Second, 4)
	_, _, err := d.Decode(context.Background(), "application/pdf", "", strings.NewReader("this is way more than four bytes"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if gotSize != 4 {
		t.Fatalf("server received %d bytes, want capped to 4", gotSize)
	}
}

func TestScriptDecoderTCP(t *testing.T) {
	srv := &scriptServer{decode: okDecodeFn("tcp extracted")}
	ln := srv.listen(t, "tcp", "127.0.0.1:0")
	defer ln.Close()

	d := newScriptDecoder(ln.Addr().String(), 5*time.Second, 0)
	text, ok, err := d.Decode(context.Background(), "application/pdf", "", strings.NewReader("bytes"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !ok || string(text) != "tcp extracted" {
		t.Fatalf("text=%q ok=%v, want %q true", text, ok, "tcp extracted")
	}
}

// TestScriptDecoderTypesPrefilterSkipsUnsupported (#697) proves the TYPES
// cache is consulted before ever dialing DECODE: a content type outside the
// advertised list is skipped locally, without a second connection.
func TestScriptDecoderTypesPrefilterSkipsUnsupported(t *testing.T) {
	sock := shortSocketPath(t)
	srv := &scriptServer{
		typesMode: "list",
		types:     []string{"application/pdf\t.pdf"},
		decode:    okDecodeFn("should never be called"),
	}
	ln := srv.listen(t, "unix", sock)
	defer ln.Close()

	d := newScriptDecoder("unix://"+sock, 5*time.Second, 0)
	_, ok, err := d.Decode(context.Background(), "application/x-nonexistent", "archive.zip", strings.NewReader("bytes"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for a type outside the TYPES list")
	}
	if n := srv.decodeCalls.Load(); n != 0 {
		t.Fatalf("DECODE dialed %d times, want 0 — prefilter should skip locally", n)
	}
}

// TestScriptDecoderTypesPrefilterDialsSupportedAndCaches proves a supported
// type (matched by extension, case-insensitively) still reaches DECODE, and
// that TYPES is only negotiated once across multiple Decode calls.
func TestScriptDecoderTypesPrefilterDialsSupportedAndCaches(t *testing.T) {
	sock := shortSocketPath(t)
	srv := &scriptServer{
		typesMode: "list",
		types:     []string{"application/pdf\t.pdf"},
		decode:    okDecodeFn("extracted"),
	}
	ln := srv.listen(t, "unix", sock)
	defer ln.Close()

	d := newScriptDecoder("unix://"+sock, 5*time.Second, 0)
	text, ok, err := d.Decode(context.Background(), "application/octet-stream", "Report.PDF", strings.NewReader("bytes"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !ok || string(text) != "extracted" {
		t.Fatalf("text=%q ok=%v, want a supported-extension match to dial DECODE", text, ok)
	}

	// A second call — regardless of type — must not re-negotiate TYPES.
	_, _, err = d.Decode(context.Background(), "application/x-nonexistent", "", strings.NewReader("bytes"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if n := srv.typesCalls.Load(); n != 1 {
		t.Fatalf("TYPES negotiated %d times, want exactly 1 (cached)", n)
	}
}

// TestScriptDecoderTypesFallbackOnErrorResponse (#697): an explicit ERROR
// reply to TYPES means the prefilter is unavailable — Decode must fall back
// to asking every part via DECODE, same as pre-#697.
func TestScriptDecoderTypesFallbackOnErrorResponse(t *testing.T) {
	sock := shortSocketPath(t)
	srv := &scriptServer{typesMode: "error", decode: okDecodeFn("asked anyway")}
	ln := srv.listen(t, "unix", sock)
	defer ln.Close()

	d := newScriptDecoder("unix://"+sock, 5*time.Second, 0)
	text, ok, err := d.Decode(context.Background(), "application/x-anything", "", strings.NewReader("bytes"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !ok || string(text) != "asked anyway" {
		t.Fatalf("text=%q ok=%v, want a fallback DECODE call", text, ok)
	}
}

// TestScriptDecoderTypesFallbackOnConnectionClose (#697): a v1 script that
// has never heard of TYPES and just closes the connection (no response at
// all) must be treated the same as an explicit ERROR — fall back to asking.
func TestScriptDecoderTypesFallbackOnConnectionClose(t *testing.T) {
	sock := shortSocketPath(t)
	srv := &scriptServer{typesMode: "close", decode: okDecodeFn("asked anyway")}
	ln := srv.listen(t, "unix", sock)
	defer ln.Close()

	d := newScriptDecoder("unix://"+sock, 5*time.Second, 0)
	text, ok, err := d.Decode(context.Background(), "application/x-anything", "", strings.NewReader("bytes"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !ok || string(text) != "asked anyway" {
		t.Fatalf("text=%q ok=%v, want a fallback DECODE call", text, ok)
	}
}

// TestScriptDecoderTypesFallbackOnTimeout (#697): a v1 script that accepts
// the TYPES connection but never writes anything back must not block Decode
// forever — the dedicated fetch times out and falls back to asking.
func TestScriptDecoderTypesFallbackOnTimeout(t *testing.T) {
	sock := shortSocketPath(t)
	srv := &scriptServer{typesMode: "hang", hangFor: time.Second, decode: okDecodeFn("asked anyway")}
	ln := srv.listen(t, "unix", sock)
	defer ln.Close()

	d := newScriptDecoder("unix://"+sock, 100*time.Millisecond, 0)
	text, ok, err := d.Decode(context.Background(), "application/x-anything", "", strings.NewReader("bytes"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !ok || string(text) != "asked anyway" {
		t.Fatalf("text=%q ok=%v, want a fallback DECODE call after the TYPES timeout", text, ok)
	}
}
