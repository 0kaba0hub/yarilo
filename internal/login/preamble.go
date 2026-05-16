package login

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"strings"
)

// preamble holds what the login pod learned from the pre-auth protocol exchange.
type preamble struct {
	username  string   // for director LOOKUP
	authLines []string // lines to replay to backend (verbatim, CRLF-terminated)
	ehloLine  string   // SMTP EHLO line replayed after XCLIENT reset (submission only)
	cmdTag    string   // IMAP command tag; empty for POP3/Submission
}

// extractPreamble dispatches to the protocol-specific handler.
// extTLS is non-nil when the login pod already upgraded to implicit TLS;
// it is passed to the STARTTLS handler so STARTTLS is only offered on plain listeners.
func extractPreamble(conn net.Conn, rd *bufio.Reader, p Protocol, extTLS *tls.Config) (*preamble, error) {
	switch p {
	case ProtocolIMAP, ProtocolIMAPS:
		return extractIMAPPreamble(conn, rd, extTLS)
	case ProtocolPOP3, ProtocolPOP3S:
		return extractPOP3Preamble(conn, rd, extTLS)
	case ProtocolSubmission, ProtocolSubmissions:
		return extractSubmissionPreamble(conn, rd, extTLS)
	default:
		return nil, fmt.Errorf("preamble: unknown protocol %q", p)
	}
}

// extractIMAPPreamble speaks minimal IMAP until the client authenticates.
// Handles: CAPABILITY, ID, NOOP, LOGOUT, STARTTLS, LOGIN, AUTHENTICATE PLAIN.
func extractIMAPPreamble(conn net.Conn, rd *bufio.Reader, extTLS *tls.Config) (*preamble, error) {
	if _, err := fmt.Fprintf(conn, "* OK Yarilo Login ready\r\n"); err != nil {
		return nil, fmt.Errorf("imap: send greeting: %w", err)
	}

	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("imap: read: %w", err)
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
			caps := "IMAP4rev1 IMAP4rev2 AUTH=PLAIN AUTH=LOGIN"
			if extTLS == nil {
				caps += " STARTTLS"
			}
			fmt.Fprintf(conn, "* CAPABILITY %s\r\n", caps)
			fmt.Fprintf(conn, "%s OK CAPABILITY\r\n", tag)
		case "ID":
			fmt.Fprintf(conn, "* ID NIL\r\n")
			fmt.Fprintf(conn, "%s OK ID\r\n", tag)
		case "NOOP":
			fmt.Fprintf(conn, "%s OK NOOP\r\n", tag)
		case "LOGOUT":
			fmt.Fprintf(conn, "* BYE Logging out\r\n")
			fmt.Fprintf(conn, "%s OK LOGOUT\r\n", tag)
			return nil, fmt.Errorf("imap: client logged out before auth")
		case "STARTTLS":
			if extTLS == nil {
				return nil, fmt.Errorf("imap: STARTTLS but no TLS config")
			}
			fmt.Fprintf(conn, "%s OK Begin TLS negotiation\r\n", tag)
			tlsConn := tls.Server(conn, extTLS)
			if err := tlsConn.Handshake(); err != nil {
				return nil, fmt.Errorf("imap: STARTTLS handshake: %w", err)
			}
			// Replace conn and rd with TLS-upgraded versions.
			conn = tlsConn
			rd = bufio.NewReaderSize(conn, 4096)
			extTLS = nil // prevent another STARTTLS offer
		case "LOGIN":
			if len(fields) < 4 {
				fmt.Fprintf(conn, "%s BAD LOGIN requires username and password\r\n", tag)
				continue
			}
			username := stripQuotes(fields[2])
			return &preamble{
				username:  username,
				authLines: []string{line + "\r\n"},
				cmdTag:    tag,
			}, nil
		case "AUTHENTICATE":
			if len(fields) < 3 {
				fmt.Fprintf(conn, "%s BAD AUTHENTICATE requires mechanism\r\n", tag)
				continue
			}
			if strings.ToUpper(fields[2]) != "PLAIN" {
				fmt.Fprintf(conn, "%s NO Unsupported mechanism\r\n", tag)
				continue
			}
			var b64 string
			if len(fields) >= 4 {
				b64 = fields[3]
			} else {
				if _, err := fmt.Fprintf(conn, "+ \r\n"); err != nil {
					return nil, fmt.Errorf("imap: send challenge: %w", err)
				}
				resp, err := rd.ReadString('\n')
				if err != nil {
					return nil, fmt.Errorf("imap: read auth: %w", err)
				}
				b64 = strings.TrimRight(resp, "\r\n")
			}
			username, err := decodePlainUsername(b64)
			if err != nil {
				fmt.Fprintf(conn, "%s BAD Invalid SASL encoding\r\n", tag)
				continue
			}
			return &preamble{
				username:  username,
				authLines: []string{fmt.Sprintf("%s AUTHENTICATE PLAIN %s\r\n", tag, b64)},
				cmdTag:    tag,
			}, nil
		default:
			fmt.Fprintf(conn, "%s BAD Not permitted before authentication\r\n", tag)
		}
	}
}

// extractPOP3Preamble speaks minimal POP3 until USER+PASS are received.
func extractPOP3Preamble(conn net.Conn, rd *bufio.Reader, extTLS *tls.Config) (*preamble, error) {
	if _, err := fmt.Fprintf(conn, "+OK Yarilo Login ready\r\n"); err != nil {
		return nil, fmt.Errorf("pop3: send greeting: %w", err)
	}

	var username string
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("pop3: read: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)

		switch {
		case upper == "CAPA":
			capa := "+OK\r\nUSER\r\nSASL PLAIN\r\n"
			if extTLS == nil {
				capa += "STLS\r\n"
			}
			capa += ".\r\n"
			fmt.Fprint(conn, capa)
		case upper == "NOOP":
			fmt.Fprintf(conn, "+OK\r\n")
		case upper == "QUIT":
			fmt.Fprintf(conn, "+OK Goodbye\r\n")
			return nil, fmt.Errorf("pop3: client quit before auth")
		case upper == "STLS":
			if extTLS == nil {
				fmt.Fprintf(conn, "-ERR TLS not available\r\n")
				continue
			}
			fmt.Fprintf(conn, "+OK Begin TLS\r\n")
			tlsConn := tls.Server(conn, extTLS)
			if err := tlsConn.Handshake(); err != nil {
				return nil, fmt.Errorf("pop3: STLS handshake: %w", err)
			}
			conn = tlsConn
			rd = bufio.NewReaderSize(conn, 4096)
			extTLS = nil
			username = ""
		case strings.HasPrefix(upper, "USER "):
			username = strings.TrimSpace(line[5:])
			fmt.Fprintf(conn, "+OK\r\n")
		case strings.HasPrefix(upper, "PASS "):
			if username == "" {
				fmt.Fprintf(conn, "-ERR No USER given\r\n")
				continue
			}
			return &preamble{
				username: username,
				authLines: []string{
					fmt.Sprintf("USER %s\r\n", username),
					line + "\r\n",
				},
			}, nil
		default:
			fmt.Fprintf(conn, "-ERR Unknown command\r\n")
		}
	}
}

// extractSubmissionPreamble speaks minimal SMTP until AUTH PLAIN/LOGIN completes.
// extTLS is used for STARTTLS on port 587; nil on port 465 (already TLS).
func extractSubmissionPreamble(conn net.Conn, rd *bufio.Reader, extTLS *tls.Config) (*preamble, error) {
	if _, err := fmt.Fprintf(conn, "220 Yarilo Login ready\r\n"); err != nil {
		return nil, fmt.Errorf("smtp: send greeting: %w", err)
	}

	var ehloLine string
	var tlsDone bool

	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("smtp: read: %w", err)
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
			caps += "250-AUTH PLAIN LOGIN\r\n"
			caps += "250 8BITMIME\r\n"
			if _, err := io.WriteString(conn, caps); err != nil {
				return nil, fmt.Errorf("smtp: send ehlo resp: %w", err)
			}
		case upper == "STARTTLS":
			if extTLS == nil || tlsDone {
				fmt.Fprintf(conn, "454 TLS not available\r\n")
				continue
			}
			fmt.Fprintf(conn, "220 Ready to start TLS\r\n")
			tlsConn := tls.Server(conn, extTLS)
			if err := tlsConn.Handshake(); err != nil {
				return nil, fmt.Errorf("smtp: STARTTLS handshake: %w", err)
			}
			conn = tlsConn
			rd = bufio.NewReaderSize(conn, 4096)
			tlsDone = true
			ehloLine = "" // must re-EHLO after STARTTLS
		case strings.HasPrefix(upper, "AUTH "):
			return handleSMTPAuth(conn, rd, trimmed, ehloLine)
		case upper == "QUIT":
			fmt.Fprintf(conn, "221 Bye\r\n")
			return nil, fmt.Errorf("smtp: client quit before auth")
		case upper == "NOOP":
			fmt.Fprintf(conn, "250 OK\r\n")
		case upper == "RSET":
			fmt.Fprintf(conn, "250 OK\r\n")
		default:
			fmt.Fprintf(conn, "503 5.5.1 AUTH required\r\n")
		}
	}
}

// handleSMTPAuth processes AUTH PLAIN or AUTH LOGIN and returns the preamble.
func handleSMTPAuth(conn net.Conn, rd *bufio.Reader, line, ehloLine string) (*preamble, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		fmt.Fprintf(conn, "501 5.5.4 Syntax error\r\n")
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
		username, err := decodePlainUsername(b64)
		if err != nil {
			fmt.Fprintf(conn, "535 5.7.8 Authentication failed\r\n")
			return nil, fmt.Errorf("smtp: plain decode: %w", err)
		}
		return &preamble{
			username:  username,
			authLines: []string{fmt.Sprintf("AUTH PLAIN %s\r\n", b64)},
			ehloLine:  ehloLine,
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
			fmt.Fprintf(conn, "535 5.7.8 Authentication failed\r\n")
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
			fmt.Fprintf(conn, "535 5.7.8 Authentication failed\r\n")
			return nil, fmt.Errorf("smtp: login password decode: %w", err)
		}
		// Re-encode as PLAIN for backend replay: authzid\0authcid\0passwd
		plainB64 := base64.StdEncoding.EncodeToString(
			append([]byte("\x00"+username+"\x00"), passBytes...),
		)
		return &preamble{
			username:  username,
			authLines: []string{fmt.Sprintf("AUTH PLAIN %s\r\n", plainB64)},
			ehloLine:  ehloLine,
		}, nil

	default:
		fmt.Fprintf(conn, "504 5.7.4 Authentication mechanism not supported\r\n")
		return nil, fmt.Errorf("smtp: unsupported auth mechanism %q", mech)
	}
}

// decodePlainUsername extracts authcid from a SASL PLAIN base64 payload.
// Wire format: [authzid] NUL authcid NUL passwd
func decodePlainUsername(b64str string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(b64str)
	if err != nil {
		return "", fmt.Errorf("base64: %w", err)
	}
	parts := bytes.SplitN(data, []byte{0}, 3)
	if len(parts) != 3 {
		return "", fmt.Errorf("PLAIN: expected 3 NUL-separated parts")
	}
	authcid := string(parts[1])
	if authcid == "" {
		return "", fmt.Errorf("PLAIN: empty authcid")
	}
	return authcid, nil
}

func stripQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
