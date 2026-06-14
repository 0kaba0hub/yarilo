package login

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
)

// imapPreAuthCaps returns the IMAP capability string for the pre-auth state.
// extTLS is non-nil when STARTTLS is available (plain listener).
func imapPreAuthCaps(extTLS *tls.Config, opts Options) string {
	caps := "IMAP4rev2 IMAP4rev1 SASL-IR LITERAL- ID IDLE"
	if extTLS != nil {
		caps += " STARTTLS"
	}
	// Plain-text mechanisms: suppressed on unencrypted connections when
	// DisablePlainAuth is set; always offered once TLS is established.
	if !opts.DisablePlainAuth || extTLS == nil {
		caps += " AUTH=PLAIN AUTH=LOGIN"
	}
	if opts.OAuth2Enabled {
		caps += " AUTH=OAUTHBEARER AUTH=XOAUTH2"
	}
	return caps
}

// preamble holds what the login pod learned from the pre-auth protocol exchange.
type preamble struct {
	username string // for director LOOKUP and yarilo-auth AUTH
	password string // credential to pass to yarilo-auth AUTH
	ehloLine string // SMTP EHLO line replayed after XCLIENT reset (submission only)
	cmdTag   string // IMAP command tag; empty for POP3/Submission
}

// extractPreamble dispatches to the protocol-specific handler.
// Returns the preamble and the (possibly TLS-upgraded) conn and rd — STARTTLS
// inside the handler replaces the plain conn, so callers must use the returned
// values for all subsequent writes to the client.
func extractPreamble(conn net.Conn, rd *bufio.Reader, p Protocol, extTLS *tls.Config, opts Options) (*preamble, net.Conn, *bufio.Reader, error) {
	switch p {
	case ProtocolIMAP, ProtocolIMAPS:
		return extractIMAPPreamble(conn, rd, extTLS, opts)
	case ProtocolPOP3, ProtocolPOP3S:
		return extractPOP3Preamble(conn, rd, extTLS, opts)
	case ProtocolSubmission, ProtocolSubmissions:
		return extractSubmissionPreamble(conn, rd, extTLS, opts)
	default:
		return nil, conn, rd, fmt.Errorf("preamble: unknown protocol %q", p)
	}
}

// extractIMAPPreamble sends the greeting then enters the auth command loop.
func extractIMAPPreamble(conn net.Conn, rd *bufio.Reader, extTLS *tls.Config, opts Options) (*preamble, net.Conn, *bufio.Reader, error) {
	caps := imapPreAuthCaps(extTLS, opts)
	if _, err := fmt.Fprintf(conn, "* OK [CAPABILITY %s] Yarilo Login ready\r\n", caps); err != nil {
		return nil, conn, rd, fmt.Errorf("imap: send greeting: %w", err)
	}
	return imapCommandLoop(conn, rd, extTLS, opts)
}

// imapCommandLoop handles IMAP commands until the client sends credentials.
// Does NOT send the greeting — call extractIMAPPreamble for the initial exchange
// or continueAuth for retry after a failed authentication.
// Returns the updated conn and rd (may be TLS-upgraded if STARTTLS was performed).
func imapCommandLoop(conn net.Conn, rd *bufio.Reader, extTLS *tls.Config, opts Options) (*preamble, net.Conn, *bufio.Reader, error) {
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return nil, conn, rd, fmt.Errorf("imap: read: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		tag := fields[0]
		cmd := strings.ToUpper(fields[1])

		switch cmd {
		case "CAPABILITY":
			c := imapPreAuthCaps(extTLS, opts)
			fmt.Fprintf(conn, "* CAPABILITY %s\r\n", c)                       //nolint:errcheck
			fmt.Fprintf(conn, "%s OK [CAPABILITY %s] CAPABILITY\r\n", tag, c) //nolint:errcheck
		case "ID":
			fmt.Fprintf(conn, "* ID NIL\r\n")      //nolint:errcheck
			fmt.Fprintf(conn, "%s OK ID\r\n", tag) //nolint:errcheck
		case "NOOP":
			fmt.Fprintf(conn, "%s OK NOOP\r\n", tag) //nolint:errcheck
		case "LOGOUT":
			fmt.Fprintf(conn, "* BYE Logging out\r\n") //nolint:errcheck
			fmt.Fprintf(conn, "%s OK LOGOUT\r\n", tag) //nolint:errcheck
			return nil, conn, rd, fmt.Errorf("imap: client logged out before auth")
		case "STARTTLS":
			if extTLS == nil {
				// RFC 3501 §6.2.1 and RFC 9051 §6.2.1: BAD when TLS is not available.
				fmt.Fprintf(conn, "%s BAD STARTTLS not available\r\n", tag) //nolint:errcheck
				continue
			}
			fmt.Fprintf(conn, "%s OK Begin TLS negotiation\r\n", tag) //nolint:errcheck
			tlsConn := tls.Server(conn, extTLS)
			if err := tlsConn.Handshake(); err != nil {
				return nil, conn, rd, fmt.Errorf("imap: STARTTLS handshake: %w", err)
			}
			conn = tlsConn
			rd = bufio.NewReaderSize(conn, 4096)
			extTLS = nil // prevent another STARTTLS offer
		case "LOGIN":
			username, password, loginErr := parseIMAPLoginArgs(line, rd, conn)
			if loginErr != nil {
				fmt.Fprintf(conn, "%s BAD LOGIN requires username and password\r\n", tag) //nolint:errcheck
				continue
			}
			return &preamble{
				username: username,
				password: password,
				cmdTag:   tag,
			}, conn, rd, nil
		case "AUTHENTICATE":
			if len(fields) < 3 {
				fmt.Fprintf(conn, "%s BAD AUTHENTICATE requires mechanism\r\n", tag) //nolint:errcheck
				continue
			}
			switch strings.ToUpper(fields[2]) {
			case "PLAIN":
				var b64 string
				if len(fields) >= 4 {
					b64 = fields[3]
				}
				// "=" means empty initial response (RFC 4959) — treat as no initial
				// response and request the client to send credentials.
				if b64 == "" || b64 == "=" {
					if _, err := fmt.Fprintf(conn, "+ \r\n"); err != nil {
						return nil, conn, rd, fmt.Errorf("imap: send challenge: %w", err)
					}
					resp, err := rd.ReadString('\n')
					if err != nil {
						return nil, conn, rd, fmt.Errorf("imap: read auth: %w", err)
					}
					b64 = strings.TrimRight(resp, "\r\n")
				}
				username, password, err := decodePlainCreds(b64)
				if err != nil {
					fmt.Fprintf(conn, "%s BAD Invalid SASL encoding\r\n", tag) //nolint:errcheck
					continue
				}
				return &preamble{
					username: username,
					password: password,
					cmdTag:   tag,
				}, conn, rd, nil
			case "LOGIN":
				// Two-step: server prompts for username, then password.
				if _, err := fmt.Fprintf(conn, "+ VXNlcm5hbWU6\r\n"); err != nil {
					return nil, conn, rd, fmt.Errorf("imap: auth login username prompt: %w", err)
				}
				userB64, err := rd.ReadString('\n')
				if err != nil {
					return nil, conn, rd, fmt.Errorf("imap: auth login username: %w", err)
				}
				userB64 = strings.TrimRight(userB64, "\r\n")
				userBytes, decErr := base64.StdEncoding.DecodeString(userB64)
				if decErr != nil {
					fmt.Fprintf(conn, "%s BAD Invalid base64\r\n", tag) //nolint:errcheck
					continue
				}
				username := string(userBytes)
				if _, err := fmt.Fprintf(conn, "+ UGFzc3dvcmQ6\r\n"); err != nil {
					return nil, conn, rd, fmt.Errorf("imap: auth login password prompt: %w", err)
				}
				passB64, err := rd.ReadString('\n')
				if err != nil {
					return nil, conn, rd, fmt.Errorf("imap: auth login password: %w", err)
				}
				passB64 = strings.TrimRight(passB64, "\r\n")
				passBytes, decErr := base64.StdEncoding.DecodeString(passB64)
				if decErr != nil {
					fmt.Fprintf(conn, "%s BAD Invalid base64\r\n", tag) //nolint:errcheck
					continue
				}
				return &preamble{
					username: username,
					password: string(passBytes),
					cmdTag:   tag,
				}, conn, rd, nil
			default:
				fmt.Fprintf(conn, "%s NO Unsupported mechanism\r\n", tag) //nolint:errcheck
				continue
			}
		default:
			fmt.Fprintf(conn, "%s BAD Not permitted before authentication\r\n", tag) //nolint:errcheck
		}
	}
}

// extractPOP3Preamble sends the greeting then enters the auth command loop.
func extractPOP3Preamble(conn net.Conn, rd *bufio.Reader, extTLS *tls.Config, opts Options) (*preamble, net.Conn, *bufio.Reader, error) {
	if _, err := fmt.Fprintf(conn, "+OK Yarilo Login ready\r\n"); err != nil {
		return nil, conn, rd, fmt.Errorf("pop3: send greeting: %w", err)
	}
	return pop3CommandLoop(conn, rd, extTLS, opts)
}

// pop3CommandLoop handles POP3 commands until USER+PASS or AUTH PLAIN/LOGIN are received.
// Does NOT send the greeting — call extractPOP3Preamble for the initial exchange
// or continueAuth for retry after a failed authentication.
func pop3CommandLoop(conn net.Conn, rd *bufio.Reader, extTLS *tls.Config, opts Options) (*preamble, net.Conn, *bufio.Reader, error) {
	var username string
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return nil, conn, rd, fmt.Errorf("pop3: read: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)

		switch {
		case upper == "CAPA":
			capa := "+OK\r\nCAPA\r\nTOP\r\nUIDL\r\nRESP-CODES\r\nPIPELINING\r\nAUTH-RESP-CODE\r\n"
			if extTLS != nil {
				capa += "STLS\r\n"
			}
			if !opts.DisablePlainAuth || extTLS == nil {
				capa += "USER\r\nSASL PLAIN LOGIN\r\n"
			}
			if opts.OAuth2Enabled {
				capa += "SASL OAUTHBEARER XOAUTH2\r\n"
			}
			capa += ".\r\n"
			fmt.Fprint(conn, capa) //nolint:errcheck
		case upper == "NOOP":
			fmt.Fprintf(conn, "+OK\r\n") //nolint:errcheck
		case upper == "QUIT":
			fmt.Fprintf(conn, "+OK Goodbye\r\n") //nolint:errcheck
			return nil, conn, rd, fmt.Errorf("pop3: client quit before auth")
		case upper == "STLS":
			if extTLS == nil {
				fmt.Fprintf(conn, "-ERR TLS not available\r\n") //nolint:errcheck
				continue
			}
			fmt.Fprintf(conn, "+OK Begin TLS\r\n") //nolint:errcheck
			tlsConn := tls.Server(conn, extTLS)
			if err := tlsConn.Handshake(); err != nil {
				return nil, conn, rd, fmt.Errorf("pop3: STLS handshake: %w", err)
			}
			conn = tlsConn
			rd = bufio.NewReaderSize(conn, 4096)
			extTLS = nil
			username = ""
		case strings.HasPrefix(upper, "AUTH"):
			fields := strings.Fields(line)
			if len(fields) < 2 {
				fmt.Fprintf(conn, "-ERR Unknown authentication mechanism\r\n") //nolint:errcheck
				continue
			}
			switch strings.ToUpper(fields[1]) {
			case "PLAIN":
				var b64 string
				if len(fields) >= 3 {
					b64 = fields[2]
				} else {
					if _, err := fmt.Fprintf(conn, "+ \r\n"); err != nil {
						return nil, conn, rd, fmt.Errorf("pop3: auth plain challenge: %w", err)
					}
					resp, err := rd.ReadString('\n')
					if err != nil {
						return nil, conn, rd, fmt.Errorf("pop3: auth plain response: %w", err)
					}
					b64 = strings.TrimRight(resp, "\r\n")
				}
				user, pass, decErr := decodePlainCreds(b64)
				if decErr != nil {
					fmt.Fprintf(conn, "-ERR Invalid authentication\r\n") //nolint:errcheck
					continue
				}
				return &preamble{username: user, password: pass}, conn, rd, nil
			case "LOGIN":
				if _, err := fmt.Fprintf(conn, "+ VXNlcm5hbWU6\r\n"); err != nil {
					return nil, conn, rd, fmt.Errorf("pop3: auth login username prompt: %w", err)
				}
				userB64, err := rd.ReadString('\n')
				if err != nil {
					return nil, conn, rd, fmt.Errorf("pop3: auth login username: %w", err)
				}
				userBytes, decErr := base64.StdEncoding.DecodeString(strings.TrimRight(userB64, "\r\n"))
				if decErr != nil {
					fmt.Fprintf(conn, "-ERR Invalid base64\r\n") //nolint:errcheck
					continue
				}
				if _, err := fmt.Fprintf(conn, "+ UGFzc3dvcmQ6\r\n"); err != nil {
					return nil, conn, rd, fmt.Errorf("pop3: auth login password prompt: %w", err)
				}
				passB64, err := rd.ReadString('\n')
				if err != nil {
					return nil, conn, rd, fmt.Errorf("pop3: auth login password: %w", err)
				}
				passBytes, decErr := base64.StdEncoding.DecodeString(strings.TrimRight(passB64, "\r\n"))
				if decErr != nil {
					fmt.Fprintf(conn, "-ERR Invalid base64\r\n") //nolint:errcheck
					continue
				}
				return &preamble{username: string(userBytes), password: string(passBytes)}, conn, rd, nil
			default:
				fmt.Fprintf(conn, "-ERR Unknown authentication mechanism\r\n") //nolint:errcheck
				continue
			}
		case strings.HasPrefix(upper, "USER "):
			username = strings.TrimSpace(line[5:])
			fmt.Fprintf(conn, "+OK\r\n") //nolint:errcheck
		case strings.HasPrefix(upper, "PASS "):
			if username == "" {
				fmt.Fprintf(conn, "-ERR No USER given\r\n") //nolint:errcheck
				continue
			}
			return &preamble{
				username: username,
				password: strings.TrimSpace(line[5:]),
			}, conn, rd, nil
		default:
			fmt.Fprintf(conn, "-ERR Unknown command\r\n") //nolint:errcheck
		}
	}
}

// continueAuth re-enters the protocol command loop after a failed authentication
// without re-sending the greeting. Used to keep the connection alive for retries.
// STARTTLS is re-offered when extTLS is non-nil (i.e. the connection is still plain).
func continueAuth(conn net.Conn, rd *bufio.Reader, extTLS *tls.Config, p Protocol, opts Options) (*preamble, net.Conn, *bufio.Reader, error) {
	switch p {
	case ProtocolIMAP, ProtocolIMAPS:
		return imapCommandLoop(conn, rd, extTLS, opts)
	case ProtocolPOP3, ProtocolPOP3S:
		return pop3CommandLoop(conn, rd, extTLS, opts)
	default:
		return nil, conn, rd, fmt.Errorf("login: continueAuth: non-retriable protocol %q", p)
	}
}

// extractSubmissionPreamble speaks minimal SMTP until AUTH PLAIN/LOGIN completes.
// extTLS is used for STARTTLS on port 587; nil on port 465 (already TLS).
// Returns the updated conn and rd (may be TLS-upgraded after STARTTLS).
func extractSubmissionPreamble(conn net.Conn, rd *bufio.Reader, extTLS *tls.Config, opts Options) (*preamble, net.Conn, *bufio.Reader, error) {
	if _, err := fmt.Fprintf(conn, "220 Yarilo Login ready\r\n"); err != nil {
		return nil, conn, rd, fmt.Errorf("smtp: send greeting: %w", err)
	}

	var ehloLine string
	var tlsDone bool

	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return nil, conn, rd, fmt.Errorf("smtp: read: %w", err)
		}
		trimmed := strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(trimmed)

		switch {
		case strings.HasPrefix(upper, "EHLO ") || strings.HasPrefix(upper, "HELO "):
			ehloLine = trimmed + "\r\n"
			caps := fmt.Sprintf("250-%s Hello\r\n", strings.Fields(trimmed)[1])
			if extTLS != nil && !tlsDone {
				caps += "250-STARTTLS\r\n"
			}
			if !opts.DisablePlainAuth || extTLS == nil || tlsDone {
				caps += "250-AUTH PLAIN LOGIN"
				if opts.OAuth2Enabled {
					caps += " OAUTHBEARER XOAUTH2"
				}
				caps += "\r\n"
			} else if opts.OAuth2Enabled {
				caps += "250-AUTH OAUTHBEARER XOAUTH2\r\n"
			}
			caps += "250 8BITMIME\r\n"
			if _, err := io.WriteString(conn, caps); err != nil {
				return nil, conn, rd, fmt.Errorf("smtp: send ehlo resp: %w", err)
			}
		case upper == "STARTTLS":
			if extTLS == nil || tlsDone {
				fmt.Fprintf(conn, "454 TLS not available\r\n") //nolint:errcheck
				continue
			}
			fmt.Fprintf(conn, "220 Ready to start TLS\r\n") //nolint:errcheck
			tlsConn := tls.Server(conn, extTLS)
			if err := tlsConn.Handshake(); err != nil {
				return nil, conn, rd, fmt.Errorf("smtp: STARTTLS handshake: %w", err)
			}
			conn = tlsConn
			rd = bufio.NewReaderSize(conn, 4096)
			tlsDone = true
			ehloLine = "" // must re-EHLO after STARTTLS
		case strings.HasPrefix(upper, "AUTH "):
			pre, err := handleSMTPAuth(conn, rd, trimmed, ehloLine)
			if err != nil {
				return nil, conn, rd, err
			}
			return pre, conn, rd, nil
		case upper == "QUIT":
			fmt.Fprintf(conn, "221 Bye\r\n") //nolint:errcheck
			return nil, conn, rd, fmt.Errorf("smtp: client quit before auth")
		case upper == "NOOP":
			fmt.Fprintf(conn, "250 OK\r\n") //nolint:errcheck
		case upper == "RSET":
			fmt.Fprintf(conn, "250 OK\r\n") //nolint:errcheck
		default:
			fmt.Fprintf(conn, "503 5.5.1 AUTH required\r\n") //nolint:errcheck
		}
	}
}

// handleSMTPAuth processes AUTH PLAIN or AUTH LOGIN and returns the preamble.
func handleSMTPAuth(conn net.Conn, rd *bufio.Reader, line, ehloLine string) (*preamble, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		fmt.Fprintf(conn, "501 5.5.4 Syntax error\r\n") //nolint:errcheck
		return nil, fmt.Errorf("smtp: malformed AUTH")
	}
	mech := strings.ToUpper(fields[1])

	switch mech {
	case "PLAIN":
		var b64 string
		if len(fields) >= 3 {
			b64 = fields[2]
		} else {
			// Challenge-response
			if _, err := fmt.Fprintf(conn, "334 \r\n"); err != nil {
				return nil, fmt.Errorf("smtp: plain challenge: %w", err)
			}
			resp, err := rd.ReadString('\n')
			if err != nil {
				return nil, fmt.Errorf("smtp: plain response: %w", err)
			}
			b64 = strings.TrimRight(resp, "\r\n")
		}
		username, password, err := decodePlainCreds(b64)
		if err != nil {
			fmt.Fprintf(conn, "535 5.7.8 Authentication failed\r\n") //nolint:errcheck
			return nil, fmt.Errorf("smtp: plain decode: %w", err)
		}
		return &preamble{
			username: username,
			password: password,
			ehloLine: ehloLine,
		}, nil

	case "LOGIN":
		// Two-step: server prompts for username, then password.
		if _, err := fmt.Fprintf(conn, "334 VXNlcm5hbWU6\r\n"); err != nil { // "Username:"
			return nil, fmt.Errorf("smtp: login username prompt: %w", err)
		}
		userB64, err := rd.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("smtp: login username: %w", err)
		}
		userB64 = strings.TrimRight(userB64, "\r\n")
		userBytes, err := base64.StdEncoding.DecodeString(userB64)
		if err != nil {
			fmt.Fprintf(conn, "535 5.7.8 Authentication failed\r\n") //nolint:errcheck
			return nil, fmt.Errorf("smtp: login username decode: %w", err)
		}
		username := string(userBytes)

		if _, err := fmt.Fprintf(conn, "334 UGFzc3dvcmQ6\r\n"); err != nil { // "Password:"
			return nil, fmt.Errorf("smtp: login password prompt: %w", err)
		}
		passB64, err := rd.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("smtp: login password: %w", err)
		}
		passB64 = strings.TrimRight(passB64, "\r\n")
		passBytes, err := base64.StdEncoding.DecodeString(passB64)
		if err != nil {
			fmt.Fprintf(conn, "535 5.7.8 Authentication failed\r\n") //nolint:errcheck
			return nil, fmt.Errorf("smtp: login password decode: %w", err)
		}
		return &preamble{
			username: username,
			password: string(passBytes),
			ehloLine: ehloLine,
		}, nil

	default:
		fmt.Fprintf(conn, "504 5.7.4 Authentication mechanism not supported\r\n") //nolint:errcheck
		return nil, fmt.Errorf("smtp: unsupported auth mechanism %q", mech)
	}
}

// decodePlainCreds extracts authcid and passwd from a SASL PLAIN base64 payload.
// Wire format: [authzid] NUL authcid NUL passwd
func decodePlainCreds(b64str string) (username, password string, err error) {
	data, decErr := base64.StdEncoding.DecodeString(b64str)
	if decErr != nil {
		return "", "", fmt.Errorf("base64: %w", decErr)
	}
	parts := bytes.SplitN(data, []byte{0}, 3)
	if len(parts) != 3 {
		return "", "", fmt.Errorf("PLAIN: expected 3 NUL-separated parts")
	}
	authcid := string(parts[1])
	if authcid == "" {
		return "", "", fmt.Errorf("PLAIN: empty authcid")
	}
	return authcid, string(parts[2]), nil
}

// decodePlainUsername extracts only the authcid from a SASL PLAIN base64 payload.
func decodePlainUsername(b64str string) (string, error) {
	u, _, err := decodePlainCreds(b64str)
	return u, err
}

// parseIMAPLoginArgs parses username and password from an IMAP LOGIN command
// line, handling RFC 3501 quoted strings, atoms, and synchronizing /
// non-synchronizing literals ({N}, {N+}, {N-}).
func parseIMAPLoginArgs(line string, rd *bufio.Reader, conn net.Conn) (username, password string, err error) {
	s := line
	i := strings.IndexByte(s, ' ')
	if i < 0 {
		return "", "", fmt.Errorf("imap/login: missing tag")
	}
	s = strings.TrimLeft(s[i:], " ")
	i = strings.IndexByte(s, ' ')
	if i < 0 {
		return "", "", fmt.Errorf("imap/login: missing command")
	}
	s = strings.TrimLeft(s[i:], " ")

	username, s, err = readIMAPString(s, rd, conn)
	if err != nil {
		return
	}
	s = strings.TrimLeft(s, " ")
	if s == "" {
		// username was a literal — password follows on the next read
		var next string
		next, err = rd.ReadString('\n')
		if err != nil {
			return "", "", fmt.Errorf("imap/login: read password segment: %w", err)
		}
		s = strings.TrimLeft(strings.TrimRight(next, "\r\n"), " ")
	}
	password, _, err = readIMAPString(s, rd, conn)
	return
}

// readIMAPString reads one IMAP string token (quoted string, literal, or atom)
// from s. Literals cause reads from rd; synchronizing literals send a
// continuation response on conn before reading. Returns the value, the
// remaining inline text, and any I/O error.
func readIMAPString(s string, rd *bufio.Reader, conn net.Conn) (val, rest string, err error) {
	if s == "" {
		return "", "", fmt.Errorf("imap/login: expected string token, got empty")
	}
	if s[0] == '{' {
		end := strings.IndexByte(s, '}')
		if end < 0 {
			return "", "", fmt.Errorf("imap/login: malformed literal")
		}
		sizeStr := s[1:end]
		sync := true
		if strings.HasSuffix(sizeStr, "+") || strings.HasSuffix(sizeStr, "-") {
			sizeStr = sizeStr[:len(sizeStr)-1]
			sync = false
		}
		n, parseErr := strconv.Atoi(sizeStr)
		if parseErr != nil || n < 0 || n > 65536 {
			return "", "", fmt.Errorf("imap/login: literal size invalid: %q", s[1:end])
		}
		if sync {
			if _, werr := fmt.Fprintf(conn, "+ go ahead\r\n"); werr != nil {
				return "", "", fmt.Errorf("imap/login: send continuation: %w", werr)
			}
		}
		buf := make([]byte, n)
		if _, ioErr := io.ReadFull(rd, buf); ioErr != nil {
			return "", "", fmt.Errorf("imap/login: read literal: %w", ioErr)
		}
		return string(buf), s[end+1:], nil
	}
	if s[0] == '"' {
		var buf strings.Builder
		i := 1
		for i < len(s) {
			if s[i] == '\\' && i+1 < len(s) {
				buf.WriteByte(s[i+1])
				i += 2
				continue
			}
			if s[i] == '"' {
				return buf.String(), s[i+1:], nil
			}
			buf.WriteByte(s[i])
			i++
		}
		return "", "", fmt.Errorf("imap/login: unterminated quoted string")
	}
	// atom: read until whitespace
	i := strings.IndexAny(s, " \t\r\n")
	if i < 0 {
		return s, "", nil
	}
	return s[:i], s[i:], nil
}
