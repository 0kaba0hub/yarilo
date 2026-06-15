package login

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
)

// manageSieveGreeting sends the pre-auth capability block to the client.
func manageSieveGreeting(conn net.Conn, extTLS *tls.Config, opts Options) error {
	var mechs string
	if !opts.DisablePlainAuth || extTLS == nil {
		mechs = "PLAIN LOGIN"
	}
	if _, err := fmt.Fprintf(conn, "%q %q\r\n", "IMPLEMENTATION", "yarilo ManageSieve"); err != nil {
		return err
	}
	if opts.SieveExtensions != "" {
		if _, err := fmt.Fprintf(conn, "%q %q\r\n", "SIEVE", opts.SieveExtensions); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(conn, "%q %q\r\n", "NOTIFY", "mailto"); err != nil {
		return err
	}
	if mechs != "" {
		if _, err := fmt.Fprintf(conn, "%q %q\r\n", "SASL", mechs); err != nil {
			return err
		}
	}
	if extTLS != nil {
		if _, err := fmt.Fprintf(conn, "%q\r\n", "STARTTLS"); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(conn, "%q %q\r\n", "VERSION", "1.0"); err != nil {
		return err
	}
	_, err := fmt.Fprintf(conn, "OK \"ManageSieve ready.\"\r\n")
	return err
}

// extractManageSievePreamble sends the greeting then handles the pre-auth
// command loop (CAPABILITY, AUTHENTICATE, STARTTLS, NOOP, LOGOUT) until
// the client authenticates successfully.
func extractManageSievePreamble(conn net.Conn, rd *bufio.Reader, extTLS *tls.Config, opts Options) (*preamble, net.Conn, *bufio.Reader, error) {
	if err := manageSieveGreeting(conn, extTLS, opts); err != nil {
		return nil, conn, rd, fmt.Errorf("managesieve: greeting: %w", err)
	}
	return manageSieveCommandLoop(conn, rd, extTLS, opts)
}

func manageSieveCommandLoop(conn net.Conn, rd *bufio.Reader, extTLS *tls.Config, opts Options) (*preamble, net.Conn, *bufio.Reader, error) {
	maxInvalid := opts.SieveMaxInvalidCmds
	if maxInvalid <= 0 {
		maxInvalid = 3
	}
	invalidCmds := 0
	for {
		cmd, err := msReadAtom(rd)
		if err != nil {
			return nil, conn, rd, fmt.Errorf("managesieve: read command: %w", err)
		}
		if cmd == "" {
			msSkipLine(rd)
			continue
		}
		switch strings.ToUpper(cmd) {
		case "CAPABILITY":
			msSkipLine(rd)
			if err := manageSieveGreeting(conn, extTLS, opts); err != nil {
				return nil, conn, rd, err
			}
		case "AUTHENTICATE":
			pre, err := msHandleAuthenticate(conn, rd)
			if err != nil {
				return nil, conn, rd, err
			}
			if pre != nil {
				return pre, conn, rd, nil
			}
			// pre == nil means auth failed; loop continues (client may retry)
		case "STARTTLS":
			msSkipLine(rd)
			if extTLS == nil {
				fmt.Fprintf(conn, "NO \"STARTTLS not available.\"\r\n") //nolint:errcheck
				continue
			}
			fmt.Fprintf(conn, "OK \"Begin TLS negotiation.\"\r\n") //nolint:errcheck
			tlsConn := tls.Server(conn, extTLS)
			if err := tlsConn.Handshake(); err != nil {
				return nil, conn, rd, fmt.Errorf("managesieve: STARTTLS handshake: %w", err)
			}
			conn = tlsConn
			rd = bufio.NewReaderSize(conn, 4096)
			extTLS = nil // prevent a second STARTTLS offer
			if err := manageSieveGreeting(conn, nil, opts); err != nil {
				return nil, conn, rd, err
			}
		case "NOOP":
			msSkipLine(rd)
			fmt.Fprintf(conn, "OK\r\n") //nolint:errcheck
		case "LOGOUT":
			msSkipLine(rd)
			fmt.Fprintf(conn, "BYE \"Logging out.\"\r\n") //nolint:errcheck
			return nil, conn, rd, fmt.Errorf("managesieve: client logged out before auth")
		default:
			msSkipLine(rd)
			invalidCmds++
			if invalidCmds >= maxInvalid {
				fmt.Fprintf(conn, "BYE \"Too many invalid MANAGESIEVE commands.\"\r\n") //nolint:errcheck
				return nil, conn, rd, fmt.Errorf("managesieve: too many invalid commands")
			}
			fmt.Fprintf(conn, "NO \"Unknown command: %s\"\r\n", cmd) //nolint:errcheck
		}
	}
}

// msHandleAuthenticate processes AUTHENTICATE mechanism [initial-response].
// Returns a non-nil *preamble on success; nil on auth failure (caller retries).
func msHandleAuthenticate(conn net.Conn, rd *bufio.Reader) (*preamble, error) {
	mechBytes, err := msReadString(rd, conn)
	if err != nil {
		msSkipLine(rd)
		fmt.Fprintf(conn, "NO \"Bad mechanism.\"\r\n") //nolint:errcheck
		return nil, nil
	}
	mech := strings.ToUpper(string(mechBytes))

	// Optional initial response (on same line for quoted; may be a literal).
	msSkipWS(rd)
	b, err := rd.ReadByte()
	if err != nil {
		return nil, fmt.Errorf("managesieve: auth read: %w", err)
	}

	var initResp []byte
	if b == '\r' || b == '\n' {
		// No initial response on this line.
		if b == '\r' {
			_, _ = rd.ReadByte() // consume LF
		}
		initResp = nil
	} else {
		_ = rd.UnreadByte()
		initResp, err = msReadLastArg(rd, conn)
		if err != nil {
			fmt.Fprintf(conn, "NO \"Bad initial response.\"\r\n") //nolint:errcheck
			return nil, nil
		}
	}

	switch mech {
	case "PLAIN":
		return msHandlePlain(conn, rd, initResp)
	case "LOGIN":
		return msHandleLogin(conn, rd)
	default:
		fmt.Fprintf(conn, "NO \"Unsupported mechanism.\"\r\n") //nolint:errcheck
		return nil, nil
	}
}

func msHandlePlain(conn net.Conn, rd *bufio.Reader, initResp []byte) (*preamble, error) {
	var b64 string
	if initResp != nil {
		b64 = string(initResp)
	} else {
		// Two-step: send empty challenge, read client response.
		fmt.Fprintf(conn, "%q\r\n", "") //nolint:errcheck
		resp, err := msReadLastArg(rd, conn)
		if err != nil {
			fmt.Fprintf(conn, "NO \"Bad response.\"\r\n") //nolint:errcheck
			return nil, nil
		}
		b64 = string(resp)
	}

	username, password, err := decodePlainCreds(b64)
	if err != nil {
		fmt.Fprintf(conn, "NO (AUTHENTICATIONFAILED) \"Invalid credentials.\"\r\n") //nolint:errcheck
		return nil, nil
	}
	return &preamble{username: username, password: password}, nil
}

func msHandleLogin(conn net.Conn, rd *bufio.Reader) (*preamble, error) {
	// Challenge 1: username
	if _, err := fmt.Fprintf(conn, "%q\r\n", base64.StdEncoding.EncodeToString([]byte("Username:"))); err != nil {
		return nil, fmt.Errorf("managesieve: login challenge: %w", err)
	}
	userB64, err := msReadLastArg(rd, conn)
	if err != nil {
		fmt.Fprintf(conn, "NO \"Bad username.\"\r\n") //nolint:errcheck
		return nil, nil
	}
	username, err := base64.StdEncoding.DecodeString(string(userB64))
	if err != nil {
		fmt.Fprintf(conn, "NO (AUTHENTICATIONFAILED) \"Invalid credentials.\"\r\n") //nolint:errcheck
		return nil, nil
	}

	// Challenge 2: password
	if _, err := fmt.Fprintf(conn, "%q\r\n", base64.StdEncoding.EncodeToString([]byte("Password:"))); err != nil {
		return nil, fmt.Errorf("managesieve: login challenge: %w", err)
	}
	passB64, err := msReadLastArg(rd, conn)
	if err != nil {
		fmt.Fprintf(conn, "NO \"Bad password.\"\r\n") //nolint:errcheck
		return nil, nil
	}
	password, err := base64.StdEncoding.DecodeString(string(passB64))
	if err != nil {
		fmt.Fprintf(conn, "NO (AUTHENTICATIONFAILED) \"Invalid credentials.\"\r\n") //nolint:errcheck
		return nil, nil
	}
	return &preamble{username: string(username), password: string(password)}, nil
}

// msReadAtom reads a ManageSieve command token (up to whitespace or EOL).
func msReadAtom(rd *bufio.Reader) (string, error) {
	msSkipWS(rd)
	var sb strings.Builder
	for {
		b, err := rd.ReadByte()
		if err != nil {
			if sb.Len() > 0 {
				break // end of token at connection close
			}
			return "", err
		}
		if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
			_ = rd.UnreadByte()
			break
		}
		sb.WriteByte(b)
	}
	return sb.String(), nil
}

// msSkipWS skips space and tab.
func msSkipWS(rd *bufio.Reader) {
	for {
		b, err := rd.ReadByte()
		if err != nil {
			return
		}
		if b != ' ' && b != '\t' {
			_ = rd.UnreadByte()
			return
		}
	}
}

// msSkipLine discards bytes through the next LF.
func msSkipLine(rd *bufio.Reader) {
	for {
		b, err := rd.ReadByte()
		if err != nil || b == '\n' {
			return
		}
	}
}

// msReadString reads a ManageSieve string (quoted or {N+} literal).
func msReadString(rd *bufio.Reader, conn net.Conn) ([]byte, error) {
	msSkipWS(rd)
	b, err := rd.ReadByte()
	if err != nil {
		return nil, err
	}
	switch b {
	case '"':
		return msReadQuoted(rd)
	case '{':
		return msReadLiteral(rd, conn)
	default:
		_ = rd.UnreadByte()
		return nil, fmt.Errorf("managesieve: expected string, got %q", rune(b))
	}
}

// msReadLastArg reads the final string argument (quoted or literal) and,
// for quoted strings, also consumes the trailing CRLF.
func msReadLastArg(rd *bufio.Reader, conn net.Conn) ([]byte, error) {
	msSkipWS(rd)
	b, err := rd.ReadByte()
	if err != nil {
		return nil, err
	}
	switch b {
	case '"':
		data, err := msReadQuoted(rd)
		if err != nil {
			return nil, err
		}
		msSkipLine(rd)
		return data, nil
	case '{':
		return msReadLiteral(rd, conn)
	default:
		_ = rd.UnreadByte()
		return nil, fmt.Errorf("managesieve: expected string, got %q", rune(b))
	}
}

func msReadQuoted(rd *bufio.Reader) ([]byte, error) {
	var sb strings.Builder
	for {
		b, err := rd.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("managesieve: unterminated quoted string: %w", err)
		}
		switch b {
		case '"':
			return []byte(sb.String()), nil
		case '\\':
			next, err := rd.ReadByte()
			if err != nil {
				return nil, fmt.Errorf("managesieve: bad escape: %w", err)
			}
			sb.WriteByte(next)
		default:
			sb.WriteByte(b)
		}
	}
}

func msReadLiteral(rd *bufio.Reader, conn net.Conn) ([]byte, error) {
	// '{' already consumed.
	var sizeBuf strings.Builder
	sync := true
	for {
		b, err := rd.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("managesieve: literal size: %w", err)
		}
		if b == '}' {
			break
		}
		if b == '+' {
			sync = false
			continue
		}
		sizeBuf.WriteByte(b)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(sizeBuf.String()), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("managesieve: literal size parse: %w", err)
	}
	// Consume the CRLF after {N} or {N+}.
	msSkipCRLF(rd)
	if sync && conn != nil {
		fmt.Fprintf(conn, "OK\r\n") //nolint:errcheck
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(rd, data); err != nil {
		return nil, fmt.Errorf("managesieve: literal body: %w", err)
	}
	return data, nil
}

func msSkipCRLF(rd *bufio.Reader) {
	b, err := rd.ReadByte()
	if err != nil {
		return
	}
	if b == '\r' {
		next, err := rd.ReadByte()
		if err != nil || next != '\n' {
			if err == nil {
				_ = rd.UnreadByte()
			}
		}
		return
	}
	if b != '\n' {
		_ = rd.UnreadByte()
	}
}
