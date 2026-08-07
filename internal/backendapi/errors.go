package backendapi

import "errors"

// Sentinel errors shared by handlers; usable with errors.Is.
var (
	errDecode         = errors.New("decode failed")
	errFolderRequired = errors.New("folder required")
	errFolderNotFound = errors.New("folder not found")
	errUserRequired   = errors.New("user required")
	errRootWithFolder = errors.New(`"root" and "folder" are alternatives`)
	errAllWithFolders = errors.New(`"all" and "folders" are alternatives`)
	errAttrRequired   = errors.New("attr required")
	errEntryRequired  = errors.New("entry required")

	// errMdboxRebuildUnsupported: mdbox stores messages storage-wide,
	// so per-folder rebuild would import unrelated messages (501).
	errMdboxRebuildUnsupported = errors.New("mdbox per-folder rebuild unsupported (storage-wide scan would import unrelated messages); use POST /api/backend/index/rebuild-storage")
)
