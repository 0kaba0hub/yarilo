package decoder

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// fakeScriptServerFull replies OK with the text bytes correctly framed
// (size line, then raw text bytes appended immediately after).
func fakeScriptServerFull(t *testing.T, network, addr string) net.Listener {
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
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				if _, err := readFields(r); err != nil {
					return
				}
				_ = writeLine(conn, scriptCmdVersion, scriptProtocolVersion, scriptRespOK)
				fields, err := readFields(r)
				if err != nil || len(fields) != 4 {
					return
				}
				n, _ := strconv.Atoi(fields[3])
				body := make([]byte, n)
				_, _ = readFull(r, body)
				text := "extracted text here"
				_ = writeLine(conn, scriptRespOK, strconv.Itoa(len(text)))
				_, _ = conn.Write([]byte(text))
			}()
		}
	}()
	return ln
}

func TestScriptDecoderSuccess(t *testing.T) {
	sock := shortSocketPath(t)
	ln := fakeScriptServerFull(t, "unix", sock)
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
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		_, _ = readFields(r)
		_ = writeLine(conn, scriptCmdVersion, scriptProtocolVersion, scriptRespOK)
		fields, _ := readFields(r)
		n, _ := strconv.Atoi(fields[3])
		body := make([]byte, n)
		_, _ = readFull(r, body)
		_ = writeLine(conn, scriptRespSkip)
	}()

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
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		_, _ = readFields(r)
		_ = writeLine(conn, scriptCmdVersion, scriptProtocolVersion, scriptRespOK)
		fields, _ := readFields(r)
		n, _ := strconv.Atoi(fields[3])
		body := make([]byte, n)
		_, _ = readFull(r, body)
		_ = writeLine(conn, scriptRespError, "decoder crashed")
	}()

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
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		_, _ = readFields(r)
		_ = writeLine(conn, scriptCmdVersion, scriptProtocolVersion, scriptRespOK)
		fields, _ := readFields(r)
		n, _ := strconv.Atoi(fields[3])
		gotSize = n
		body := make([]byte, n)
		_, _ = readFull(r, body)
		_ = writeLine(conn, scriptRespOK, "0")
	}()

	d := newScriptDecoder("unix://"+sock, 5*time.Second, 4)
	_, _, err = d.Decode(context.Background(), "application/pdf", "", strings.NewReader("this is way more than four bytes"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if gotSize != 4 {
		t.Fatalf("server received %d bytes, want capped to 4", gotSize)
	}
}

func TestScriptDecoderTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		_, _ = readFields(r)
		_ = writeLine(conn, scriptCmdVersion, scriptProtocolVersion, scriptRespOK)
		fields, _ := readFields(r)
		n, _ := strconv.Atoi(fields[3])
		body := make([]byte, n)
		_, _ = readFull(r, body)
		text := "tcp extracted"
		_ = writeLine(conn, scriptRespOK, strconv.Itoa(len(text)))
		_, _ = conn.Write([]byte(text))
	}()

	d := newScriptDecoder(ln.Addr().String(), 5*time.Second, 0)
	text, ok, err := d.Decode(context.Background(), "application/pdf", "", strings.NewReader("bytes"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !ok || string(text) != "tcp extracted" {
		t.Fatalf("text=%q ok=%v, want %q true", text, ok, "tcp extracted")
	}
}
