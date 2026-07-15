package quota

// Policy carries the site-wide quota tunables that layer on top of the
// per-user quota_rule limits: percentage scaling, extra headroom, delivery
// grace, and the structural mailbox-count caps. All are global config (not
// per-user rules), mirroring the quota plugin settings.
type Policy struct {
	// StoragePercentage scales the resolved storage limit (limit*pct/100).
	// 100 = no scaling. Must be > 0.
	StoragePercentage int
	// MessagePercentage scales the resolved message-count limit. 100 = none.
	MessagePercentage int
	// StorageExtra is byte headroom added to the storage limit after the
	// percentage scaling. 0 = none.
	StorageExtra int64
	// StorageGrace is the byte overshoot allowed past the storage limit, applied
	// ONLY on inbound delivery (LMTP/LDA), never interactive IMAP. 0 = none.
	StorageGrace int64
	// IgnoreUnlimited omits the quota root from IMAP GETQUOTA/GETQUOTAROOT for a
	// user whose limits are all unlimited.
	IgnoreUnlimited bool
	// MailboxCount caps the number of mailboxes (folders) a user may have.
	// 0 = unlimited. Enforced at folder creation.
	MailboxCount int64
	// MailboxMessageCount caps the number of messages in a single mailbox.
	// 0 = unlimited. Enforced on save.
	MailboxMessageCount int64
}

// Unlimited reports whether the limits impose no storage and no message cap.
func (l Limits) Unlimited() bool { return l.StorageBytes == 0 && l.Messages == 0 }

// pct returns a percentage value, defaulting a non-positive one to 100 so a
// zero-valued Policy is a no-op rather than scaling every limit to zero.
func pct(p int) int {
	if p <= 0 {
		return 100
	}
	return p
}

// Scale applies StoragePercentage/MessagePercentage and StorageExtra to a
// resolved limit set. Unlimited resources (0) stay unlimited. Callers pass the
// already-per-folder-resolved Limits (from Limits.EffectiveLimits).
func (p Policy) Scale(l Limits) Limits {
	out := l
	if l.StorageBytes > 0 {
		b := l.StorageBytes
		if sp := pct(p.StoragePercentage); sp != 100 {
			b = int64(float64(b) / 100.0 * float64(sp))
		}
		b += p.StorageExtra
		if b < 0 {
			b = 0
		}
		out.StorageBytes = b
	}
	if l.Messages > 0 {
		if mp := pct(p.MessagePercentage); mp != 100 {
			out.Messages = int64(float64(l.Messages) / 100.0 * float64(mp))
		}
	}
	return out
}

// IsOverWithGrace reports whether adding newBytes / newMsgs would exceed the
// limits, allowing storageGrace bytes of overshoot on the storage resource
// only. Message count gets no grace. storageGrace 0 makes this identical to
// IsOver.
func IsOverWithGrace(u Usage, limits Limits, newBytes, newMsgs, storageGrace int64) bool {
	if limits.StorageBytes > 0 && u.StorageBytes+newBytes > limits.StorageBytes+storageGrace {
		return true
	}
	if limits.Messages > 0 && u.Messages+newMsgs > limits.Messages {
		return true
	}
	return false
}
