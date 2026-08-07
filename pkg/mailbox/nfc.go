package mailbox

import "golang.org/x/text/unicode/norm"

// NFC returns the NFC form of s. It is a no-op for already-NFC input, so
// applying it twice costs a scan and changes nothing.
//
// It lives here rather than in a driver because normalisation is a property of
// turning a logical folder name into a storage name, and every tree derived
// from that name -- mail, index, volatile, ACL, FTS -- has to reach the same
// answer. It did not: the drivers normalised before building their path and the
// four derived trees passed the logical name straight through, so one mailbox
// addressed in two Unicode forms was one directory in the mail tree and two in
// each of the others (#1092).
func NFC(s string) string { return norm.NFC.String(s) }
