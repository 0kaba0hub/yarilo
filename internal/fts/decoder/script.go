package decoder

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Wire protocol — TAB-delimited, LF-terminated, same style as pkg/locks (see
// docs/DEPLOYMENT.md §Wire protocol). A fresh connection per Decode call: the
// decoder script is expected to be a lightweight, stateless per-request
// process, and attachment decoding is rare enough relative to mail delivery
// that connection-pooling would be premature complexity.
//
//	> VERSION\tyarilo-fts-decoder\t1\n
//	< VERSION\t1\tOK\n
//
//	> DECODE\t<content-type>\t<filename>\t<size>\n
//	> <size raw bytes of the attachment>
//	< OK\t<text-size>\n
//	< <text-size bytes of extracted text>
//	  or
//	< SKIP\n                              — content type/extension unsupported
//	< ERROR\t<message>\n
//
// TYPES is an optional extension (#697), queried once per scriptDecoder on
// its own connection and cached: a supported-types prefilter avoids dialing
// (and shipping attachment bytes) for parts the decoder would just SKIP
// anyway.
//
//	> TYPES\n
//	< <content-type>\t<ext>\t<ext>...\n  (repeated, any number of lines)
//	< \n                                  — empty line terminates the list
//	  or
//	< ERROR\t<message>\n                  — TYPES not supported
//
// A script that doesn't recognize TYPES at all (a v1 decoder predating
// #697) may respond with ERROR, close the connection, or simply never
// respond — all three are treated identically as "prefilter unavailable":
// fall back to asking per part via DECODE/SKIP, exactly the pre-#697
// behaviour.
const (
	scriptProtocolVersion = "1"
	scriptCmdVersion      = "VERSION"
	scriptCmdDecode       = "DECODE"
	scriptCmdTypes        = "TYPES"
	scriptRespOK          = "OK"
	scriptRespSkip        = "SKIP"
	scriptRespError       = "ERROR"
)

type scriptDecoder struct {
	addr    string
	timeout time.Duration
	maxSize int64

	typesOnce sync.Once
	// supported holds lowercased content-types and extensions (with the
	// leading dot, e.g. ".pdf") the decoder advertised via TYPES. nil means
	// the prefilter is unavailable — every part is shipped to DECODE, same
	// as before #697.
	supported map[string]bool
}

func newScriptDecoder(addr string, timeout time.Duration, maxSize int64) *scriptDecoder {
	return &scriptDecoder{addr: addr, timeout: timeout, maxSize: maxSize}
}

// dial connects to addr, accepting "unix:///path/to.sock" (standalone, a
// co-located decoder process) or a bare "host:port" (k8s/backend, the
// decoder runs as its own Deployment/Service) — mirrors pkg/locks' embedded-
// vs-remote split, since a hardcoded transport doesn't fit both topologies.
func (d *scriptDecoder) dial(ctx context.Context) (net.Conn, error) {
	var dialer net.Dialer
	if path, ok := strings.CutPrefix(d.addr, "unix://"); ok {
		return dialer.DialContext(ctx, "unix", path)
	}
	return dialer.DialContext(ctx, "tcp", d.addr)
}

// ensureTypesLoaded fetches and caches the TYPES prefilter exactly once per
// scriptDecoder (#697): concurrent Decode calls during the first fetch block
// on typesOnce, same as any one-time init.
func (d *scriptDecoder) ensureTypesLoaded(ctx context.Context) {
	d.typesOnce.Do(func() {
		d.supported = d.fetchSupportedTypes(ctx)
	})
}

// fetchSupportedTypes queries TYPES on a dedicated connection. Any failure —
// an explicit ERROR response, the connection dropping, or the dedicated
// timeout expiring while a v1 script that doesn't recognize TYPES just sits
// on the connection — returns nil, meaning "prefilter unavailable": Decode
// falls back to asking per part via DECODE/SKIP, the pre-#697 behaviour.
func (d *scriptDecoder) fetchSupportedTypes(ctx context.Context) map[string]bool {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	conn, err := d.dial(ctx)
	if err != nil {
		return nil
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	r := bufio.NewReader(conn)
	if err := writeLine(conn, scriptCmdVersion, "yarilo-fts-decoder", scriptProtocolVersion); err != nil {
		return nil
	}
	hs, err := readFields(r)
	if err != nil || len(hs) < 3 || hs[0] != scriptCmdVersion || hs[2] != scriptRespOK {
		return nil
	}
	if err := writeLine(conn, scriptCmdTypes); err != nil {
		return nil
	}

	types := make(map[string]bool)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil // dropped/timed out mid-list: treat the whole fetch as unavailable
		}
		line = strings.TrimRight(line, "\n")
		if line == "" {
			return types // empty line terminates the list (possibly empty — still a success)
		}
		fields := strings.Split(line, "\t")
		if fields[0] == scriptRespError {
			return nil
		}
		for _, f := range fields {
			if f != "" {
				types[strings.ToLower(f)] = true
			}
		}
	}
}

// typeSupported reports whether the cached TYPES list covers contentType or
// filename's extension. Only meaningful when d.supported != nil — callers
// must check that first (nil means "prefilter unavailable", not "nothing
// supported").
func (d *scriptDecoder) typeSupported(contentType, filename string) bool {
	if contentType != "" && d.supported[strings.ToLower(contentType)] {
		return true
	}
	if ext := path.Ext(filename); ext != "" && d.supported[strings.ToLower(ext)] {
		return true
	}
	return false
}

func (d *scriptDecoder) Decode(ctx context.Context, contentType, filename string, body io.Reader) ([]byte, bool, error) {
	d.ensureTypesLoaded(ctx)
	if d.supported != nil && !d.typeSupported(contentType, filename) {
		return nil, false, nil
	}

	data, err := io.ReadAll(body)
	if err != nil {
		return nil, false, fmt.Errorf("fts/decoder/script: read attachment: %w", err)
	}
	if d.maxSize > 0 && int64(len(data)) > d.maxSize {
		data = data[:d.maxSize]
	}

	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	conn, err := d.dial(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("fts/decoder/script: dial %s: %w", d.addr, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	r := bufio.NewReader(conn)
	if err := writeLine(conn, scriptCmdVersion, "yarilo-fts-decoder", scriptProtocolVersion); err != nil {
		return nil, false, fmt.Errorf("fts/decoder/script: send version: %w", err)
	}
	hs, err := readFields(r)
	if err != nil {
		return nil, false, fmt.Errorf("fts/decoder/script: read version: %w", err)
	}
	if len(hs) < 3 || hs[0] != scriptCmdVersion || hs[2] != scriptRespOK {
		return nil, false, fmt.Errorf("fts/decoder/script: handshake mismatch %v", hs)
	}

	if err := writeLine(conn, scriptCmdDecode, contentType, filename, strconv.Itoa(len(data))); err != nil {
		return nil, false, fmt.Errorf("fts/decoder/script: send decode: %w", err)
	}
	if _, err := conn.Write(data); err != nil {
		return nil, false, fmt.Errorf("fts/decoder/script: send body: %w", err)
	}

	resp, err := readFields(r)
	if err != nil {
		return nil, false, fmt.Errorf("fts/decoder/script: read response: %w", err)
	}
	if len(resp) == 0 {
		return nil, false, fmt.Errorf("fts/decoder/script: empty response")
	}
	switch resp[0] {
	case scriptRespOK:
		if len(resp) != 2 {
			return nil, false, fmt.Errorf("fts/decoder/script: malformed OK response %v", resp)
		}
		n, err := strconv.Atoi(resp[1])
		if err != nil || n < 0 {
			return nil, false, fmt.Errorf("fts/decoder/script: bad text size %q", resp[1])
		}
		text := make([]byte, n)
		if _, err := io.ReadFull(r, text); err != nil {
			return nil, false, fmt.Errorf("fts/decoder/script: read text: %w", err)
		}
		return text, true, nil
	case scriptRespSkip:
		return nil, false, nil
	case scriptRespError:
		return nil, false, fmt.Errorf("fts/decoder/script: server error: %s", strings.Join(resp[1:], " "))
	default:
		return nil, false, fmt.Errorf("fts/decoder/script: unexpected response %v", resp)
	}
}

func writeLine(w io.Writer, fields ...string) error {
	_, err := io.WriteString(w, strings.Join(fields, "\t")+"\n")
	return err
}

func readFields(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && line == "" {
			return nil, io.ErrUnexpectedEOF
		}
		if !errors.Is(err, io.EOF) {
			return nil, err
		}
	}
	line = strings.TrimRight(line, "\n")
	if line == "" {
		return nil, fmt.Errorf("fts/decoder/script: empty line")
	}
	return strings.Split(line, "\t"), nil
}
