package mailbox

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrPathIsRoot marks a refusal to carry out a destructive operation whose path
// resolved onto a root rather than onto a folder inside one.
var ErrPathIsRoot = errors.New("mailbox: path is a storage root")

// GuardDestructivePath refuses a path that is a root, or outside one, before a
// driver removes or renames it.
//
// This is not a second copy of the name rules in ValidateName. It asks a
// different question, at the last moment before the filesystem call, and it
// asks it of the resolved path rather than of the name: whatever the name was,
// and whoever validated it, does this operation land on the mailbox itself?
//
// It exists because "the caller checked" is a promise, not a property. Once
// validation moved above the drivers (#1069), that promise became the only
// thing between a mistake and an account -- and #1063 is what that costs when
// the promise is not kept: a cleanup that named the wrong thing removed a
// user's mail and reported success.
//
// A guard here can never be the primary defence: it fires after a name has
// already been accepted everywhere else, so anything it catches is a defect
// somewhere above. That is the point -- it turns a silent one into a loud one.
// Scope is deliberate: Delete and Rename only. Save, Move and Remove name a
// message inside a folder rather than the folder itself, so the worst a bad
// name does there is create or touch a stray file -- recoverable, and already
// refused by the name rules above. Delete and Rename are the two that can
// remove a mailbox, which is the loss this exists to make loud. Widening it
// would cost a path comparison on every message write for no case it can catch.
//
// alsoRoots are further paths that are the mailbox rather than a folder in it.
// maildir needs this: with an explicit mail_path the INBOX directory can sit
// outside the folder root entirely, and refusing it as "outside the root" would
// describe the wrong fault -- it is not misplaced, it is the mailbox.
func GuardDestructivePath(root, path string, alsoRoots ...string) error {
	if root == "" {
		// Nothing to compare against; a driver that cannot say where its root
		// is cannot be guarded, and pretending otherwise would be worse.
		return nil
	}
	cleanRoot := filepath.Clean(root)
	cleanPath := filepath.Clean(path)

	if cleanPath == cleanRoot {
		return fmt.Errorf("%w: %q is the storage root itself", ErrPathIsRoot, cleanPath)
	}
	for _, other := range alsoRoots {
		if other != "" && cleanPath == filepath.Clean(other) {
			return fmt.Errorf("%w: %q is the mailbox itself, not a folder in it", ErrPathIsRoot, cleanPath)
		}
	}
	if !strings.HasPrefix(cleanPath, cleanRoot+string(filepath.Separator)) {
		return fmt.Errorf("%w: %q is outside %q", ErrPathIsRoot, cleanPath, cleanRoot)
	}
	return nil
}

// GuardDestructivePaths applies GuardDestructivePath to several paths under one
// root, for an operation that touches more than one -- Rename has a source and
// a destination, and either landing on the root is the same fault.
func GuardDestructivePaths(root string, paths ...string) error {
	for _, p := range paths {
		if err := GuardDestructivePath(root, p); err != nil {
			return err
		}
	}
	return nil
}
