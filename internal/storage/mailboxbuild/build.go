// Package mailboxbuild is the single place a MailboxBackend is constructed from
// storage config. Every binary — the session servers (internal/backend), the
// operator API (yarilo-backend-api) and the indexer (yarilo-fts) — builds mdbox
// (and the other drivers) through ByDriver, so the per-driver tuning (alt storage,
// rotate size/interval, preallocate) can never drift between them (#639).
//
// It depends only on the storage drivers and config, not on any session-server
// package, so the lean operator/indexer binaries do not pull in imap/lmtp code.
package mailboxbuild

import (
	"strconv"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
	"github.com/yarilomail/yarilo/pkg/quota"
)

// ByDriver constructs a MailboxBackend for the named driver from sc, applying
// every configured tunable. Unknown/empty drivers default to maildir so an
// operator typo does not crash startup.
func ByDriver(driver string, sc config.StorageConfig, locker locks.Locker) mailbox.MailboxBackend {
	// Every binary builds its backend here, so wrapping at this point is what
	// makes folder-name validation unbypassable: IMAP, LMTP (Sieve fileinto
	// names the folder from the user's own script), POP3, JMAP, ManageSieve
	// and the backend API all receive a checked backend without having to
	// remember to ask for one (#1069).
	return mailbox.Validating(byDriver(driver, sc, locker), mailbox.NameRules{
		ValidateFSNames:       sc.MailboxListValidateFSNames,
		RefuseLayoutSeparator: sc.MailboxListRefuseLayoutSeparator,
		ReservedSegments:      sc.MailboxListReservedSegments,
		StorageEscapeChar:     sc.MailboxListStorageEscapeChar,
	})
}

func byDriver(driver string, sc config.StorageConfig, locker locks.Locker) mailbox.MailboxBackend {
	switch strings.ToLower(driver) {
	case "sdbox", "dbox":
		return dboxv2.New(dboxv2.WithLocker(locker), dboxv2.WithMaxConcurrentWrites(sc.MaxConcurrentWrites),
			dboxv2.WithListUTF8(sc.MailboxListUTF8), dboxv2.WithNormalizeNFC(sc.MailboxListNormalizeToNFC))
	case "mdbox":
		return mdbox.New(mdbox.WithLocker(locker), mdbox.WithAltStorage(sc.MdboxAltStoragePath),
			mdbox.WithMaxConcurrentWrites(sc.MaxConcurrentWrites),
			mdbox.WithListUTF8(sc.MailboxListUTF8), mdbox.WithNormalizeNFC(sc.MailboxListNormalizeToNFC),
			mdbox.WithRotateSize(uint32(quota.ParseSize(sc.MdboxRotateSize))),
			mdbox.WithRotateInterval(time.Duration(ParseIntervalSeconds(sc.MdboxRotateInterval))*time.Second),
			mdbox.WithPreallocate(sc.MdboxPreallocateSpace))
	default:
		return maildir.New(maildir.WithLocker(locker), maildir.WithMaxConcurrentWrites(sc.MaxConcurrentWrites),
			maildir.WithListUTF8(sc.MailboxListUTF8), maildir.WithNormalizeNFC(sc.MailboxListNormalizeToNFC))
	}
}

// ParseIntervalSeconds converts a duration string ("30s", "5m", "1h") or a bare
// second count ("30") into whole seconds. Empty, "0", or an unparseable value
// yields 0 (disabled) — the same lenient contract as quota.ParseSize, so a
// malformed knob degrades to the safe default rather than failing startup.
func ParseIntervalSeconds(s string) int {
	if s == "" || s == "0" {
		return 0
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 {
			return 0
		}
		return n
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return int(d.Seconds())
	}
	return 0
}
