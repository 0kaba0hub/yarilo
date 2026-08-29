package imap

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	imapserver "github.com/emersion/go-imap/v2/imapserver"

	"github.com/yarilomail/yarilo/pkg/mailbox"
	"github.com/yarilomail/yarilo/pkg/quota"
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
// usageAfterDelta answers the post-commit usage from the cached total plus the
// change this session just made, when the cache is fresh enough to build on.
//
// The full count opens every folder of the account -- 24 us each, so 1.01 ms
// across the 42 folders of a real mailbox, and the message count does not
// enter into it. Run on every committed change, as the warning and clone path
// did, fifty clients expunging in one folder produce fifty account-wide sweeps
// and some two thousand folder-lock acquisitions per round (#1548).
//
// The session does not need the sweep to know the answer: it knows what it just
// removed or added. Only somebody else's change has to be discovered, and that
// is what the cache TTL is for.
//
// The timestamp is deliberately not refreshed. Extending it on every delta
// would keep the cache alive for as long as the load lasts and another
// session's change would never arrive; leaving it means the total is rebuilt a
// second after its last real count, so the drift this introduces cannot
// outlive the TTL.
func (s *session) usageAfterDelta(dBytes, dMessages int64) (quota.Usage, bool) {
	if s.quotaCacheAt.IsZero() || time.Since(s.quotaCacheAt) >= quotaCacheTTL {
		return quota.Usage{}, false
	}
	u := s.quotaCacheUsage
	u.StorageBytes += dBytes
	u.Messages += dMessages
	if u.StorageBytes < 0 {
		u.StorageBytes = 0
	}
	if u.Messages < 0 {
		u.Messages = 0
	}
	s.quotaCacheUsage = u
	return u, true
}

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
	// Lazy quota_over_status: reconcile on the first quota operation. evalOverStatus
	// takes u directly, so there is no re-entry into countUsage.
	if os := s.quotaPolicy().OverStatus; os.Mask != "" && os.LazyCheck && !s.overStatusChecked {
		s.evalOverStatus(u)
	}
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

// quotaPolicy returns the site-wide quota tunables.
func (s *session) quotaPolicy() quota.Policy {
	return s.srv.opts.QuotaPolicy
}

// evalOverStatus runs the quota_over_status check once per session: compares the
// actual over-quota state (from usage u) against the flag carried in userdb and,
// on a mismatch, runs the configured program to update the external flag. Guards
// match the reference: mask must be set, run once, and only while the userdb
// flag is still fresh (login within the max delay).
func (s *session) evalOverStatus(u quota.Usage) {
	os := s.quotaPolicy().OverStatus
	if os.Mask == "" || s.overStatusChecked || s.userInfo == nil {
		return
	}
	if time.Since(s.overStatusLoginAt) > overStatusMaxDelay {
		return // stale userdb flag — do not act on it
	}
	s.overStatusChecked = true
	flagged := s.userInfo.QuotaOverFlag != "" &&
		quota.WildcardMatchIcase(s.userInfo.QuotaOverFlag, os.Mask)
	actual := quota.IsOverAny(u, s.quotaPolicy().Scale(s.quotaLimits()))
	if actual != flagged {
		s.srv.opts.QuotaWarner.FireOverStatus(
			s.userInfo.Username, s.userInfo.Home, os.Execute, s.userInfo.QuotaOverFlag, actual)
	}
}

// overStatusMaxDelay bounds how long after login the userdb over-flag is still
// trusted for the quota_over_status check (mirrors the reference 10s guard).
const overStatusMaxDelay = 10 * time.Second

// captureQuotaSnap forces the quota_warning "before" baseline to the current
// usage. Called immediately before a mutating op (expunge) so the crossing
// detection has a correct pre-op value even on a delete-only session — the
// SELECT-time seed can miss (usage not yet readable, or the snapshot reset
// between SELECT and the delete). No-op without warnings configured.
func (s *session) captureQuotaSnap() {
	if len(s.quotaPolicy().Warnings) == 0 {
		return
	}
	if u, err := s.countUsage(false); err == nil {
		s.quotaSnap, s.quotaSnapSet = u, true
	}
}

// seedQuotaWarnSnap captures a baseline usage snapshot when none exists yet, so
// a quota_warning "under" crossing fires on a delete-only session (EXPUNGE with
// no prior save to seed the "before" side). No-op without warnings configured.
func (s *session) seedQuotaWarnSnap() {
	if s.quotaSnapSet {
		return
	}
	s.captureQuotaSnap()
}

// fireQuotaWarnings evaluates quota_warning crossings for the transition from
// the captured pre-op usage snapshot to after, running any matched actions.
// No-op when no warnings are configured or no snapshot has been captured yet.
func (s *session) fireQuotaWarnings(after quota.Usage) {
	pol := s.quotaPolicy()
	if len(pol.Warnings) == 0 || !s.quotaSnapSet || s.userInfo == nil {
		return
	}
	before := s.quotaSnap
	s.quotaSnap = after
	limits := pol.Scale(s.quotaLimits())
	s.srv.opts.QuotaWarner.Fire(s.userInfo.Username, s.userInfo.Home, pol.Warnings, limits, before, after)
}

// effectiveLimits resolves the per-folder limits then applies the site policy
// (percentage scaling + storage extra). Grace is delivery-only and never added
// on the interactive IMAP path.
func (s *session) effectiveLimits(folder string) (quota.Limits, bool) {
	lim, ignore := s.quotaLimits().EffectiveLimits(folder)
	if ignore {
		return lim, true
	}
	return s.quotaPolicy().Scale(lim), false
}

// cloneMirror updates the quota_clone mirror with usage u, debounced: it writes
// at most once per flush delay and otherwise defers the latest usage to the
// final flush on session close. Mirrors the reference plugin's 10s flush timer.
func (s *session) cloneMirror(u quota.Usage) {
	if s.srv.opts.QuotaClone == nil || s.userInfo == nil {
		return
	}
	if time.Since(s.cloneLastFlush) >= s.srv.opts.QuotaCloneFlushDelay {
		s.cloneFlush(u)
		return
	}
	s.cloneDirtyUsg, s.cloneDirty = u, true
}

// cloneFlush writes u to the clone dicts now and resets the debounce state.
func (s *session) cloneFlush(u quota.Usage) {
	s.cloneLastFlush = time.Now()
	s.cloneDirty = false
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.srv.opts.QuotaClone.Write(ctx, s.userInfo.Username, u)
}

// cloneFlushFinal writes any deferred usage on session close.
func (s *session) cloneFlushFinal() {
	if s.srv.opts.QuotaClone != nil && s.cloneDirty && s.userInfo != nil {
		s.cloneFlush(s.cloneDirtyUsg)
	}
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
	limits := s.quotaPolicy().Scale(s.quotaLimits())
	// quota_hidden hides the root from every user; quota_ignore_unlimited hides
	// it only for users whose limits are all unlimited.
	if s.quotaPolicy().Hidden || (limits.Unlimited() && s.quotaPolicy().IgnoreUnlimited) {
		return &imaplib.QuotaRootData{Mailbox: mailbox}, nil
	}
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
	limits := s.quotaPolicy().Scale(s.quotaLimits())
	if s.quotaPolicy().Hidden || (limits.Unlimited() && s.quotaPolicy().IgnoreUnlimited) {
		qd := imaplib.QuotaData{Name: root}
		return &qd, nil
	}
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
	// Per-mailbox message-count cap is structural (independent of quota_rule):
	// reject when the target folder would reach the configured message count.
	if mmc := s.quotaPolicy().MailboxMessageCount; mmc > 0 {
		if cur, ok := s.folderMessageCount(folder); ok && cur+1 >= mmc {
			return &imaplib.Error{
				Type: imaplib.StatusResponseTypeNo,
				Code: imaplib.ResponseCode("OVERQUOTA"),
				Text: "Too many messages in the mailbox",
			}
		}
	}
	effLim, ignore := s.effectiveLimits(folder)
	if ignore || effLim.Unlimited() {
		return nil
	}
	u, err := s.countUsage(false)
	if err != nil {
		return nil // fail-open: don't block on a transient index read error
	}
	// Capture the pre-save usage as the "before" side for quota_warning
	// crossing detection; the post-commit hook supplies "after".
	s.quotaSnap, s.quotaSnapSet = u, true
	// Grace is delivery-only; interactive IMAP saves get no overshoot.
	if quota.IsOver(u, effLim, bytes, 1) {
		return &imaplib.Error{
			Type: imaplib.StatusResponseTypeNo,
			Code: imaplib.ResponseCode("OVERQUOTA"),
			Text: s.quotaExceededMessage(),
		}
	}
	return nil
}

// folderMessageCount returns the current message count of folder from the
// index (the authoritative count backend). ok is false when the folder or
// index is unavailable.
func (s *session) folderMessageCount(folder string) (int64, bool) {
	if s.box == nil || s.idx == nil {
		return 0, false
	}
	f, err := s.idx.OpenFolder(folder, 0)
	if err != nil {
		return 0, false
	}
	_, msgs, err := s.idx.FolderVSize(f.ID)
	if err != nil {
		return 0, false
	}
	return int64(msgs), true
}
