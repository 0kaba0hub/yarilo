package ftsproto

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"

	"github.com/yarilomail/yarilo/pkg/authclient"
	"github.com/yarilomail/yarilo/pkg/locks"
)

// Serve accepts connections on ln and dispatches requests into svc until ln
// is closed. Each connection is handled on its own goroutine.
func Serve(ln net.Listener, svc Service) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("ftsproto: accept: %w", err)
		}
		go handleConn(conn, svc)
	}
}

func handleConn(conn net.Conn, svc Service) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		reply := dispatch(strings.TrimRight(line, "\n"), svc)
		if _, err := conn.Write([]byte(reply + "\n")); err != nil {
			return
		}
	}
}

// no builds a plain refusal: the request was wrong, or the service failed in a
// way that will fail again.
func no(format string, args ...any) string {
	return refusal("", fmt.Sprintf(format, args...))
}

// noFor builds a refusal that classifies err, so a dependency being restarted
// crosses the wire as something a client can be told to retry. Without this an
// outage inside yarilo-fts arrived at yarilo-imap as an ordinary string and
// reached the client as a bare NO -- indistinguishable from an FTS that is
// broken for good (#1409).
func noFor(err error) string {
	if errors.Is(err, authclient.ErrUnavailable) || errors.Is(err, locks.ErrUnavailable) {
		return refusal(CodeUnavailable, err.Error())
	}
	return refusal("", err.Error())
}

// refusal is the ONE place a NO reply is built.
//
// With a code read by position, a tab inside the text is no longer cosmetic:
// it would shift the fields and turn arbitrary error text into a code, or eat
// the text after it. Errors here are wrapped chains from anywhere in the
// service, so the text is not ours to trust -- it is flattened rather than
// escaped, since a reply is one line and nothing downstream reconstructs it.
func refusal(code, text string) string {
	text = strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(text)
	if code == "" {
		return replyNO + "\t" + text
	}
	return replyNO + "\t" + code + "\t" + text
}

func parseU32(s string) (uint32, error) {
	v, err := strconv.ParseUint(s, 10, 32)
	return uint32(v), err
}

func dispatch(line string, svc Service) string {
	f := strings.Split(line, "\t")
	switch f[0] {
	case CmdVersion:
		if len(f) != 2 || f[1] != ProtocolVersion {
			return no("unsupported protocol version")
		}
		return CmdVersion + "\t" + ProtocolVersion + "\tOK"

	case CmdIndex, CmdPrepend, CmdExpunge, CmdLookup, CmdStatus, CmdRescan:
		if len(f) < 5 {
			return no("malformed %s", f[0])
		}
		user := f[1]
		mbox, err := ParseMbox(f[2], f[3], f[4])
		if err != nil {
			return noFor(err)
		}
		switch f[0] {
		case CmdIndex:
			if len(f) != 7 {
				return no("malformed INDEX")
			}
			maxUID, err1 := parseU32(f[5])
			maxRecent, err2 := strconv.Atoi(f[6])
			if err1 != nil || err2 != nil {
				return no("malformed INDEX numbers")
			}
			if err := svc.Index(user, mbox, maxUID, maxRecent); err != nil {
				slog.Debug("fts: index failed", "user", user, "folder", mbox.Name, "max_uid", maxUID, "err", err)
				return noFor(err)
			}
			slog.Debug("fts: indexed", "user", user, "folder", mbox.Name, "max_uid", maxUID, "max_recent", maxRecent)
			return replyOK
		case CmdPrepend:
			if len(f) != 6 {
				return no("malformed PREPEND")
			}
			maxUID, err := parseU32(f[5])
			if err != nil {
				return no("malformed PREPEND uid")
			}
			if err := svc.Prepend(user, mbox, maxUID); err != nil {
				slog.Debug("fts: prepend failed", "user", user, "folder", mbox.Name, "max_uid", maxUID, "err", err)
				return noFor(err)
			}
			slog.Debug("fts: prepended", "user", user, "folder", mbox.Name, "max_uid", maxUID)
			return replyOK
		case CmdExpunge:
			if len(f) != 6 {
				return no("malformed EXPUNGE")
			}
			uid, err := parseU32(f[5])
			if err != nil {
				return no("malformed EXPUNGE uid")
			}
			if err := svc.Expunge(user, mbox, uid); err != nil {
				slog.Debug("fts: expunge failed", "user", user, "folder", mbox.Name, "uid", uid, "err", err)
				return noFor(err)
			}
			slog.Debug("fts: expunged", "user", user, "folder", mbox.Name, "uid", uid)
			return replyOK
		case CmdLookup:
			if len(f) != 6 {
				return no("malformed LOOKUP")
			}
			q, err := DecodeQuery(f[5])
			if err != nil {
				return noFor(err)
			}
			res, err := svc.Lookup(user, mbox, q)
			if err != nil {
				slog.Debug("fts: lookup failed", "user", user, "folder", mbox.Name, "err", err)
				return noFor(err)
			}
			// Result counts only — never the query terms (private mail content).
			slog.Debug("fts: lookup", "user", user, "folder", mbox.Name,
				"definite", len(res.Definite), "maybe", len(res.Maybe))
			payload, err := EncodeResult(res)
			if err != nil {
				return noFor(err)
			}
			return replyOK + "\t" + payload
		case CmdStatus:
			last, sum, err := svc.Status(user, mbox)
			if err != nil {
				return noFor(err)
			}
			slog.Debug("fts: status", "user", user, "folder", mbox.Name, "last_indexed_uid", last, "checksum", sum)
			return fmt.Sprintf("%s\t%d\t%d", replyOK, last, sum)
		default: // CmdRescan
			if err := svc.Rescan(user, mbox); err != nil {
				slog.Debug("fts: rescan failed", "user", user, "folder", mbox.Name, "err", err)
				return noFor(err)
			}
			slog.Debug("fts: rescanned", "user", user, "folder", mbox.Name)
			return replyOK
		}

	case CmdOptimize:
		if len(f) != 2 {
			return no("malformed OPTIMIZE")
		}
		if err := svc.Optimize(f[1]); err != nil {
			return noFor(err)
		}
		slog.Debug("fts: optimized", "user", f[1])
		return replyOK

	default:
		slog.Debug("fts: unknown command", "cmd", f[0])
		return no("unknown command %q", f[0])
	}
}
