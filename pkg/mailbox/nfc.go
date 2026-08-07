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

// NormalizeName returns the NFC form of a folder name, or the name unchanged
// when skip is set (mailbox_list_normalize_to_nfc = false).
//
// This is the one owner of the transformation. It is called once, where a wire
// name becomes the logical name a session threads to every tree -- mail, index,
// ACL, FTS -- so path derivation never normalises and there is no second place
// for the order of NFC-against-escaping to be held by convention (#1113). The
// path builders below it (FolderSubpathEscaped, the drivers' disk-name step)
// take the name as given, already in whichever form this decided, and carry no
// name-form parameter of their own.
//
// It is placed inside each namespace resolver rather than at each use of the
// name, so the set that must remember it is "every resolver" -- a small,
// enumerable set an AST guard can check -- rather than "every use of rel".
func NormalizeName(name string, skip bool) string {
	if skip {
		return name
	}
	return NFC(name)
}
