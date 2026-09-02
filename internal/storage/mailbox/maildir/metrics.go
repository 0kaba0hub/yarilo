package maildir

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// The driver's half of the folder lock, by the call that took it.
//
// The index publishes the same count under fileindex_lock_acquired_total, and
// both take locks.MailboxKey -- the same resource. Read alone, either half
// describes a fraction of the contention on that key and reads as the whole of
// it (#1630). Named by role rather than by function, so a call moving to
// another function does not rename the number.
var metricLockAcquired = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "maildir_lock_acquired_total",
	Help: "Cross-process folder locks the maildir driver acquired, by the call that took it. Shares the resource with fileindex_lock_acquired_total; the sum of the two is what contends on one folder.",
}, []string{"site"})

const (
	lockSiteSave           = "save"            // a delivery or APPEND
	lockSiteWriteFlags     = "write-flags"     // one message's flags reaching its name
	lockSiteWriteFlagsBulk = "write-flags-all" // a whole STORE's worth at once
	lockSiteMove           = "move"            // between folders
	lockSiteCreate         = "create"
	lockSiteDelete         = "delete"
	lockSiteKeywords       = "keyword-letters" // allocating a keyword letter outside a write
	lockSiteReconcileMove  = "reconcile-move"  // new/ into cur/
	lockSiteReconcileApply = "reconcile-apply" // the index write a reconcile decided on
	lockSiteRename         = "rename"          // a folder rename, both locks
)
