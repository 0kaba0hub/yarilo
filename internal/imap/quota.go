package imap

import (
	"context"
	"log/slog"

	imaplib "github.com/emersion/go-imap/v2"
	imapserver "github.com/emersion/go-imap/v2/imapserver"

	"github.com/0kaba0hub/yarilo/pkg/quota"
)

// Ensure *session implements SessionQuota when QuotaDict is set.
// The interface assertion is made at runtime via the session cast
// in go-imap's capability.go — we implement the interface
// unconditionally so the cast succeeds; quotaEnabled() guards
// the actual operations.
var _ imapserver.SessionQuota = (*session)(nil)

func (s *session) quotaEnabled() bool {
	return s.srv.opts.QuotaDict != nil
}

func (s *session) quotaCounter() *quota.Counter {
	if s.userInfo == nil {
		return nil
	}
	return quota.NewCounter(s.srv.opts.QuotaDict, s.userInfo.Username)
}

func (s *session) quotaLimits() quota.Limits {
	if s.userInfo == nil {
		return quota.Limits{}
	}
	return quota.ParseRules(s.userInfo.QuotaRules)
}

// GetQuotaRoot implements imapserver.SessionQuota.
func (s *session) GetQuotaRoot(mailbox string) (*imaplib.QuotaRootData, error) {
	if !s.quotaEnabled() {
		return &imaplib.QuotaRootData{Mailbox: mailbox}, nil
	}
	ctr := s.quotaCounter()
	if ctr == nil {
		return &imaplib.QuotaRootData{Mailbox: mailbox}, nil
	}

	u, err := ctr.Get(context.Background())
	if err != nil {
		slog.Warn("imap: quota get failed", "user", s.userInfo.Username, "err", err)
		return &imaplib.QuotaRootData{Mailbox: mailbox}, nil
	}
	limits := s.quotaLimits()
	qd := buildQuotaData(u, limits)
	return &imaplib.QuotaRootData{
		Mailbox: mailbox,
		Roots:   []string{quota.RootName},
		Quotas:  []imaplib.QuotaData{qd},
	}, nil
}

// GetQuota implements imapserver.SessionQuota.
func (s *session) GetQuota(root string) (*imaplib.QuotaData, error) {
	if !s.quotaEnabled() {
		qd := imaplib.QuotaData{Name: root}
		return &qd, nil
	}
	ctr := s.quotaCounter()
	if ctr == nil {
		qd := imaplib.QuotaData{Name: root}
		return &qd, nil
	}
	u, err := ctr.Get(context.Background())
	if err != nil {
		slog.Warn("imap: quota get failed", "user", s.userInfo.Username, "err", err)
		qd := imaplib.QuotaData{Name: root}
		return &qd, nil
	}
	limits := s.quotaLimits()
	qd := buildQuotaData(u, limits)
	qd.Name = root
	return &qd, nil
}

// buildQuotaData constructs the IMAP QuotaData from current usage
// and resolved limits. STORAGE is in kibibytes per RFC 9208.
func buildQuotaData(u quota.Usage, lim quota.Limits) imaplib.QuotaData {
	qd := imaplib.QuotaData{Name: quota.RootName}
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

// quotaAdd increments the quota counters after a successful save.
// Errors are logged but not returned — quota counter drift is
// recoverable via admin recalc; never block a successful save.
func (s *session) quotaAdd(ctx context.Context, bytes, messages int64) {
	if !s.quotaEnabled() || s.userInfo == nil {
		return
	}
	if err := s.quotaCounter().Add(ctx, bytes, messages); err != nil {
		slog.Warn("imap: quota counter update failed",
			"user", s.userInfo.Username, "bytes", bytes, "messages", messages, "err", err)
	}
}

// quotaCheckAppend returns an IMAP error if the user is over quota
// for the given message size. Nil means the append is allowed.
func (s *session) quotaCheckAppend(ctx context.Context, bytes int64) error {
	if !s.quotaEnabled() || s.userInfo == nil {
		return nil
	}
	lim := s.quotaLimits()
	if lim.StorageBytes == 0 && lim.Messages == 0 {
		return nil
	}
	ctr := s.quotaCounter()
	u, err := ctr.Get(ctx)
	if err != nil {
		return nil // fail-open: don't block on dict error
	}
	if quota.IsOver(u, lim, bytes, 1) {
		return &imaplib.Error{
			Type: imaplib.StatusResponseTypeNo,
			Code: imaplib.ResponseCode("OVERQUOTA"),
			Text: "Quota exceeded",
		}
	}
	return nil
}
