package ftsproto

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
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

func no(format string, args ...any) string {
	return replyNO + "\t" + fmt.Sprintf(format, args...)
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
			return no("%v", err)
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
				return no("%v", err)
			}
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
				return no("%v", err)
			}
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
				return no("%v", err)
			}
			return replyOK
		case CmdLookup:
			if len(f) != 6 {
				return no("malformed LOOKUP")
			}
			q, err := DecodeQuery(f[5])
			if err != nil {
				return no("%v", err)
			}
			res, err := svc.Lookup(user, mbox, q)
			if err != nil {
				return no("%v", err)
			}
			payload, err := EncodeResult(res)
			if err != nil {
				return no("%v", err)
			}
			return replyOK + "\t" + payload
		case CmdStatus:
			last, sum, err := svc.Status(user, mbox)
			if err != nil {
				return no("%v", err)
			}
			return fmt.Sprintf("%s\t%d\t%d", replyOK, last, sum)
		default: // CmdRescan
			if err := svc.Rescan(user, mbox); err != nil {
				return no("%v", err)
			}
			return replyOK
		}

	case CmdOptimize:
		if len(f) != 2 {
			return no("malformed OPTIMIZE")
		}
		if err := svc.Optimize(f[1]); err != nil {
			return no("%v", err)
		}
		return replyOK

	default:
		slog.Debug("fts: unknown command", "cmd", f[0])
		return no("unknown command %q", f[0])
	}
}
