package director

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
)

// preamble holds what the director learned from the pre-auth protocol exchange.
type preamble struct {
	username string // extracted for consistent-hash routing
	cmdTag   string // IMAP command tag; "" for POP3/LMTP
	authLine string // bytes to replay to backend after discarding its greeting
}

// extractPreamble dispatches to the protocol-specific extractor.
func extractPreamble(conn net.Conn, rd *bufio.Reader, protocol string) (*preamble, error) {
	switch protocol {
	case "imap", "imaps":
		return extractIMAPPreamble(conn, rd)
	case "pop3", "pop3s":
		return extractPOP3Preamble(conn, rd)
	case "lmtp":
		return extractLMTPPreamble(conn, rd)
	default:
		return nil, fmt.Errorf("preamble: unknown protocol %q", protocol)
	}
}

// extractIMAPPreamble speaks minimal IMAP until the client authenticates.
// It handles CAPABILITY, ID, NOOP, LOGOUT, LOGIN, and AUTHENTICATE PLAIN.
func extractIMAPPreamble(conn net.Conn, rd *bufio.Reader) (*preamble, error) {
	if _, err := fmt.Fprintf(conn, "* OK Yarilo Proxy ready\r\n"); err != nil {
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
			fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1 AUTH=PLAIN AUTH=LOGIN\r\n")
			fmt.Fprintf(conn, "%s OK Capability\r\n", tag)
		case "ID":
			fmt.Fprintf(conn, "* ID NIL\r\n")
			fmt.Fprintf(conn, "%s OK ID\r\n", tag)
		case "NOOP":
			fmt.Fprintf(conn, "%s OK NOOP\r\n", tag)
		case "LOGOUT":
			fmt.Fprintf(conn, "* BYE\r\n")
			fmt.Fprintf(conn, "%s OK Logout\r\n", tag)
			return nil, fmt.Errorf("imap: client logged out before auth")
		case "LOGIN":
			if len(fields) < 4 {
				fmt.Fprintf(conn, "%s BAD LOGIN requires username and password\r\n", tag)
				continue
			}
			username := stripQuotes(fields[2])
			return &preamble{
				username: username,
				cmdTag:   tag,
				authLine: line + "\r\n",
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
			// Inline credentials: AUTHENTICATE PLAIN <base64>
			var b64 string
			if len(fields) >= 4 {
				b64 = fields[3]
			} else {
				// Challenge-response
				if _, err := fmt.Fprintf(conn, "+ \r\n"); err != nil {
					return nil, fmt.Errorf("imap: send challenge: %w", err)
				}
				resp, err := rd.ReadString('\n')
				if err != nil {
					return nil, fmt.Errorf("imap: read auth: %w", err)
				}
				b64 = strings.TrimRight(resp, "\r\n")
			}
			username, err := decodePlainAuth(b64)
			if err != nil {
				fmt.Fprintf(conn, "%s BAD Invalid SASL encoding\r\n", tag)
				continue
			}
			return &preamble{
				username: username,
				cmdTag:   tag,
				authLine: fmt.Sprintf("%s AUTHENTICATE PLAIN %s\r\n", tag, b64),
			}, nil
		default:
			fmt.Fprintf(conn, "%s BAD Not permitted before authentication\r\n", tag)
		}
	}
}

// extractPOP3Preamble speaks minimal POP3 until USER+PASS are received.
func extractPOP3Preamble(conn net.Conn, rd *bufio.Reader) (*preamble, error) {
	if _, err := fmt.Fprintf(conn, "+OK Yarilo Proxy ready\r\n"); err != nil {
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
			fmt.Fprintf(conn, "+OK\r\nUSER\r\nSASL PLAIN\r\n.\r\n")
		case upper == "NOOP":
			fmt.Fprintf(conn, "+OK\r\n")
		case upper == "QUIT":
			fmt.Fprintf(conn, "+OK\r\n")
			return nil, fmt.Errorf("pop3: client quit before auth")
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
				authLine: fmt.Sprintf("USER %s\r\n%s\r\n", username, line),
			}, nil
		default:
			fmt.Fprintf(conn, "-ERR Unknown command\r\n")
		}
	}
}

// extractLMTPPreamble speaks minimal LMTP until RCPT TO is received.
// The first recipient address is used as the routing key.
func extractLMTPPreamble(conn net.Conn, rd *bufio.Reader) (*preamble, error) {
	// Read LHLO
	line, err := rd.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("lmtp: read LHLO: %w", err)
	}
	lhlo := strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(strings.ToUpper(lhlo), "LHLO") {
		fmt.Fprintf(conn, "500 Expected LHLO\r\n")
		return nil, fmt.Errorf("lmtp: expected LHLO, got %q", lhlo)
	}
	fmt.Fprintf(conn, "250-yarilo\r\n250 8BITMIME\r\n")

	var buf strings.Builder
	buf.WriteString(lhlo + "\r\n")

	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("lmtp: read: %w", err)
		}
		buf.WriteString(line)
		clean := strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(clean)

		switch {
		case strings.HasPrefix(upper, "MAIL FROM:"):
			fmt.Fprintf(conn, "250 OK\r\n")
		case strings.HasPrefix(upper, "RCPT TO:"):
			addr := extractAngleAddr(clean[8:])
			if addr == "" {
				fmt.Fprintf(conn, "550 Malformed address\r\n")
				continue
			}
			return &preamble{
				username: addr,
				authLine: buf.String(),
			}, nil
		case upper == "QUIT":
			fmt.Fprintf(conn, "221 Bye\r\n")
			return nil, fmt.Errorf("lmtp: client quit before RCPT TO")
		case upper == "NOOP":
			fmt.Fprintf(conn, "250 OK\r\n")
		default:
			fmt.Fprintf(conn, "500 Unknown command\r\n")
		}
	}
}

// decodePlainAuth decodes a SASL PLAIN base64 payload and returns authcid.
// Wire format: [authzid] NUL authcid NUL passwd
func decodePlainAuth(b64str string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(b64str)
	if err != nil {
		return "", fmt.Errorf("base64: %w", err)
	}
	parts := bytes.SplitN(data, []byte{0}, 3)
	if len(parts) != 3 {
		return "", fmt.Errorf("PLAIN: expected 3 NUL-separated parts, got %d", len(parts))
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

func extractAngleAddr(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "<") || !strings.HasSuffix(s, ">") {
		return ""
	}
	return s[1 : len(s)-1]
}
