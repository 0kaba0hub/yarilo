// fts-decoder-stub is a minimal server implementing the fts_decoder_driver=script
// wire protocol (see internal/fts/decoder/script.go), for live sandbox
// verification of the attachment-decoder code path — not for production use.
// It ignores the actual attachment bytes and always returns a fixed,
// greppable text so a live SEARCH BODY / kubectl logs check can confirm the
// decoder was actually invoked end-to-end.
//
// Usage: go run . [-listen :9199]
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
)

const (
	protocolVersion = "1"
	cmdVersion      = "VERSION"
	cmdDecode       = "DECODE"
	respOK          = "OK"
	respError       = "ERROR"
)

func main() {
	listen := flag.String("listen", ":9199", "address to listen on")
	flag.Parse()

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("listen %s: %v", *listen, err)
	}
	log.Printf("fts-decoder-stub listening on %s", *listen)
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go handle(conn)
	}
}

func handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)

	hs, err := readFields(r)
	if err != nil || len(hs) < 2 || hs[0] != cmdVersion {
		log.Printf("bad handshake from %s: %v %v", conn.RemoteAddr(), hs, err)
		return
	}
	if err := writeLine(conn, cmdVersion, protocolVersion, respOK); err != nil {
		return
	}

	fields, err := readFields(r)
	if err != nil || len(fields) != 4 || fields[0] != cmdDecode {
		log.Printf("bad decode request from %s: %v %v", conn.RemoteAddr(), fields, err)
		return
	}
	contentType, filename, sizeStr := fields[1], fields[2], fields[3]
	n, err := strconv.Atoi(sizeStr)
	if err != nil || n < 0 {
		_ = writeLine(conn, respError, "bad size")
		return
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		log.Printf("read body: %v", err)
		return
	}

	text := fmt.Sprintf("FTSDECODERSTUB-MARKER decoded content_type=%s filename=%s attachment_bytes=%d",
		contentType, filename, n)
	log.Printf("decoded: content_type=%s filename=%s bytes=%d", contentType, filename, n)
	_ = writeLine(conn, respOK, strconv.Itoa(len(text)))
	_, _ = conn.Write([]byte(text))
}

func writeLine(w io.Writer, fields ...string) error {
	_, err := io.WriteString(w, strings.Join(fields, "\t")+"\n")
	return err
}

func readFields(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\n")
	if line == "" {
		return nil, fmt.Errorf("empty line")
	}
	return strings.Split(line, "\t"), nil
}
