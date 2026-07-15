package imap

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	imapserver "github.com/emersion/go-imap/v2/imapserver"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
	"github.com/0kaba0hub/yarilo/pkg/quota"
)

// quotaChanged invalidates the short-lived read cache after a quota-affecting
// operation (APPEND / COPY / MOVE / EXPUNGE) so the next GETQUOTA re-sums the
// index. Enforcement already reads fresh.
func (s *session) quotaChanged() {
	s.quotaCacheAt = time.Time{}
}

// quotaCacheTTL bounds how long a GETQUOTA display value is served from the
// per-session cache before the index is re-summed. Enforcement bypasses it.
const quotaCacheTTL = time.Second

// countUsage returns the user's quota usage summed from the index (the
// authoritative count backend): the aggregate virtual size + message count of
// every personal-namespace folder. useCache serves a recent value for GETQUOTA
// display bursts; enforcement passes false so decisions are always fresh.
func (s *session) countUsage(useCache bool) (quota.Usage, error) {
	if s.box == nil || s.idx == nil {
		return quota.Usage{}, nil
	}
	if useCache && !s.quotaCacheAt.IsZero() && time.Since(s.quotaCacheAt) < quotaCacheTTL {
		return s.quotaCacheUsage, nil
	}
	entries, err := s.box.ListFolders()
	if err != nil {
		return quota.Usage{}, err
	}
	u := quota.CountUsage(s.idx, mailbox.SelectableNames(entries), s.quotaLimits())
	s.quotaCacheUsage = u
	s.quotaCacheAt = time.Now()
	return u, nil
}

// *session implements SessionQuota unconditionally so go-imap's capability.go
// cast succeeds; quotaExtensionEnabled() gates whether QUOTA is advertised and
// GETQUOTA answers, quotaEngineEnabled() gates save-time enforcement.
var _ imapserver.SessionQuota = (*session)(nil)

// quotaExtensionEnabled gates the IMAP QUOTA extension (GETQUOTA / capability).
func (s *session) quotaExtensionEnabled() bool {
	return s.srv.opts.IMAPQuota
}

// quotaEngineEnabled gates save-time enforcement (APPEND / COPY / MOVE).
func (s *session) quotaEngineEnabled() bool {
	return s.srv.opts.QuotaEngine
}

// quotaName is the quota-root name surfaced to clients (default "User quota").
func (s *session) quotaName() string {
	if n := s.srv.opts.QuotaName; n != "" {
		return n
	}
	return quota.RootName
}

// quotaExceededMessage is the text of an over-quota rejection.
func (s *session) quotaExceededMessage() string {
	if m := s.srv.opts.QuotaExceededMessage; m != "" {
		return m
	}
	return "Quota exceeded"
}

func (s *session) quotaLimits() quota.Limits {
	if s.userInfo == nil {
		return quota.Limits{}
	}
	return quota.ParseRules(s.userInfo.QuotaRules)
}

// GetQuotaRoot implements imapserver.SessionQuota.
func (s *session) GetQuotaRoot(mailbox string) (*imaplib.QuotaRootData, error) {
	if !s.quotaExtensionEnabled() {
		return &imaplib.QuotaRootData{Mailbox: mailbox}, nil
	}
	u, err := s.countUsage(true)
	if err != nil {
		slog.Warn("imap: quota get failed", "user", s.userInfo.Username, "err", err)
		return &imaplib.QuotaRootData{Mailbox: mailbox}, nil
	}
	limits := s.quotaLimits()
	qd := buildQuotaData(u, limits, s.quotaName())
	return &imaplib.QuotaRootData{
		Mailbox: mailbox,
		Roots:   []string{s.quotaName()},
		Quotas:  []imaplib.QuotaData{qd},
	}, nil
}

// GetQuota implements imapserver.SessionQuota.
func (s *session) GetQuota(root string) (*imaplib.QuotaData, error) {
	if !s.quotaExtensionEnabled() {
		qd := imaplib.QuotaData{Name: root}
		return &qd, nil
	}
	u, err := s.countUsage(true)
	if err != nil {
		slog.Warn("imap: quota get failed", "user", s.userInfo.Username, "err", err)
		qd := imaplib.QuotaData{Name: root}
		return &qd, nil
	}
	limits := s.quotaLimits()
	qd := buildQuotaData(u, limits, s.quotaName())
	qd.Name = root
	return &qd, nil
}

// buildQuotaData constructs the IMAP QuotaData from current usage
// and resolved limits. STORAGE is in kibibytes per RFC 9208.
func buildQuotaData(u quota.Usage, lim quota.Limits, name string) imaplib.QuotaData {
	qd := imaplib.QuotaData{Name: name}
	storageUsageKiB := quota.StorageBytesToKiB(u.StorageBytes)
	storageLimitKiB := quota.StorageBytesToKiB(lim.StorageBytes)
	qd.Resources = append(qd.Resources, imaplib.QuotaResource{
		Type:  imaplib.QuotaResourceStorage,
		Usage: storageUsageKiB,
		Limit: storageLimitKiB,
	})
	if lim.Messages > 0 || u.Messages > 0 {
		qd.Resources = append(qd.Resources, imaplib.QuotaResource{
			Type:  imaplib.QuotaResourceMessage,
			Usage: uint64(u.Messages),
			Limit: uint64(lim.Messages),
		})
	}
	return qd
}

// quotaCheckAppend returns an IMAP error if the user is over quota
// for the given message size in the target folder. Nil means allowed.
// Per-folder rules (additive limits, ignore) are applied before the check.
func (s *session) quotaCheckAppend(_ context.Context, folder string, bytes int64) error {
	if !s.quotaEngineEnabled() || s.userInfo == nil {
		return nil
	}
	// Per-message size cap is independent of the usage limit and applies even
	// when no quota_rule is set. Its rejection text is distinct so a client can
	// tell "message too large" from "mailbox full".
	if ms := s.srv.opts.QuotaMailSize; ms > 0 && bytes > ms {
		return &imaplib.Error{
			Type: imaplib.StatusResponseTypeNo,
			Code: imaplib.ResponseCode("OVERQUOTA"),
			Text: fmt.Sprintf("Requested allocation size %d exceeds max mail size %d", bytes, ms),
		}
	}
	lim := s.quotaLimits()
	effLim, ignore := lim.EffectiveLimits(folder)
	if ignore {
		return nil
	}
	if effLim.StorageBytes == 0 && effLim.Messages == 0 {
		return nil
	}
	u, err := s.countUsage(false)
	if err != nil {
		return nil // fail-open: don't block on a transient index read error
	}
	if quota.IsOver(u, effLim, bytes, 1) {
		return &imaplib.Error{
			Type: imaplib.StatusResponseTypeNo,
			Code: imaplib.ResponseCode("OVERQUOTA"),
			Text: s.quotaExceededMessage(),
		}
	}
	return nil
}
