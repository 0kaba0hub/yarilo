package imap

import (
	"log/slog"
	"net/mail"
	"strings"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/yarilomail/yarilo/internal/imapthread"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Sort implements the SORT command (RFC 5256 §3): the searched messages,
// ordered by the given keys.
//
// It shares its scan with THREAD, so both select the same messages by the same
// criteria and account for unreadable ones the same way.
func (s *session) Sort(kind imapserver.NumKind, criteria []imaplib.SortCriterion, search *imaplib.SearchCriteria) ([]uint32, error) {
	slog.Debug("imap: command", "sid", s.sid, "cmd", "Sort")
	if s.folder == nil {
		return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "No mailbox selected"}
	}
	if err := s.requireRightOnSelected(mailbox.RightRead); err != nil {
		return nil, err
	}
	search = s.substituteSearchRes(search)

	msgs, err := s.scanForOrdering(kind, search, "sort", sortNeeds(criteria))
	if err != nil {
		return nil, err
	}
	return imapthread.Sort(msgs, criteria), nil
}

// addrMailbox is the addr-mailbox of the first address in an address header:
// the local part, NOT the display name (RFC 5256 §3). "John Doe <zeta@x>"
// sorts under "zeta", so a mailbox sorted by FROM does not follow the names
// its owner sees in a client's message list -- that is what the specification
// says, and clients that want otherwise ask for DISPLAYFROM (RFC 5957), which
// this server does not announce.
func addrMailbox(header string) string {
	if header == "" {
		return ""
	}
	list, err := mail.ParseAddressList(header)
	if err != nil || len(list) == 0 {
		// A header the address parser refuses still sorts: it falls back to
		// the local part of the first bracketed address, and to the empty
		// string when there is none. An empty string collates first, which is
		// what the specification asks for a header that is absent.
		return localPart(firstBracketed(header))
	}
	return localPart(list[0].Address)
}

func localPart(addr string) string {
	if at := strings.LastIndexByte(addr, '@'); at >= 0 {
		return addr[:at]
	}
	return addr
}

func firstBracketed(v string) string {
	open := strings.IndexByte(v, '<')
	if open < 0 {
		return strings.TrimSpace(v)
	}
	end := strings.IndexByte(v[open:], '>')
	if end < 0 {
		return strings.TrimSpace(v[open+1:])
	}
	return strings.TrimSpace(v[open+1 : open+end])
}
