package backendapi

import "errors"

// Sentinel errors shared by handlers. apiError stringifies them via
// Error() so the wire payload stays the same regardless of the
// constant chosen — they exist so the linter does not flag
// "constant string in error" and so future callers can errors.Is.
var (
	errDecode         = errors.New("decode failed")
	errFolderRequired = errors.New("folder required")
	errFolderNotFound = errors.New("folder not found")
	errUserRequired   = errors.New("user required")
	errAttrRequired   = errors.New("attr required")
	errEntryRequired  = errors.New("entry required")
)
