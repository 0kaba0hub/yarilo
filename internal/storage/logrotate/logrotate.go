// Package logrotate owns the thresholds that decide when an append log is
// folded into its base file.
//
// One place, because there are two logs: the per-folder index log and the
// mdbox map log. Both are configured by the same three keys, and both used to
// carry their own copy of the defaults -- which agreed until one of them was
// changed. A single key with two built-in values is half a setting: an
// operator who leaves it alone gets one cadence on one log and another on the
// other, and nothing says so.
package logrotate

import "time"

const (
	// MinSize is the floor: a log below it is never folded. Every open
	// replays the tail at roughly half a microsecond per byte, so the floor
	// is what bounds the cost of opening an account that sits between folds.
	MinSize int64 = 32 << 10

	// MaxSize folds regardless of age.
	MaxSize int64 = 1 << 20

	// MinAge keeps a fold from firing inside a burst of appends: a log that
	// is still growing is not worth folding, since it will cross the floor
	// again in a moment. So this only has to outlast a typical delivery
	// burst.
	//
	// A minute does, and five minutes was overpayment paid by the account
	// delivering mail (#1460). Measured: at five minutes an actively
	// delivering account carried its log to ~110 KB past the floor, and over
	// that interval delivery p50 went 74 -> 103 ms while p99 went 127 -> 249
	// ms. At one minute it folds once per hundred deliveries and p99 returns
	// to 128 ms, against a fold costing ~1.2 s on a ten-thousand-message
	// account -- so one delayed fold costs about what three folds do.
	MinAge = time.Minute
)
