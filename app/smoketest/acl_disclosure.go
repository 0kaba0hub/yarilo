package main

import (
	"fmt"
	"strings"
)

// checkACLDisclosure runs the shared-namespace matrix from #1085 against a live
// deployment: a peer with no rights must not be able to tell a mailbox it may
// not see from one that does not exist, and a peer that has been granted rights
// must actually reach them.
//
// It exists because that matrix was verified only in the unit harness. The one
// attempt to run it by hand was defeated by a namespace that had been silently
// dropped, and the failure looked exactly like an ACL bug for a day (#1086). A
// check that ships with the deployment cannot be defeated the same way: if the
// namespace is missing, this fails on the owner's own CREATE, at the top, with
// the prefix in the message.
//
// owner creates and grants; peer is refused or admitted. Both must exist in the
// passdb, and prefix must name a configured shared namespace (e.g. "Public/").
func checkACLDisclosure(ownerUser, ownerPass, peerUser, peerPass, prefix string) (err error) {
	folder := prefix + "SmokeAclProbe"

	owner, err := imapDial()
	if err != nil {
		return fmt.Errorf("dial (owner): %w", err)
	}
	defer owner.close()
	if err := owner.login(ownerUser, ownerPass); err != nil {
		return fmt.Errorf("login %q: %w", ownerUser, err)
	}

	// Pre-clean, then create. A CREATE that fails here means the namespace is
	// not there -- which is the failure that wasted a day, so it is named.
	_ = owner.deleteFolder(folder)
	if _, err := owner.cmd(fmt.Sprintf("CREATE %q", folder)); err != nil {
		return fmt.Errorf("CREATE %q: %w — is %q a configured namespace on this deployment?", folder, err, prefix)
	}
	// Cleanup failure is a failure of the check, not a note in the output.
	defer func() {
		err = aclCleanupResult(err, owner.deleteFolder(folder), folder, prefix)
	}()

	peer, err := imapDial()
	if err != nil {
		return fmt.Errorf("dial (peer): %w", err)
	}
	defer peer.close()
	if err := peer.login(peerUser, peerPass); err != nil {
		return fmt.Errorf("login %q: %w", peerUser, err)
	}

	if err := aclNoOracle(peer, folder, prefix); err != nil {
		return err
	}
	if err := aclGrantsAreReachable(owner, peer, folder, peerUser); err != nil {
		return err
	}
	return nil
}

// aclNoOracle asserts that every command answers a peer with no rights exactly
// as it answers for a mailbox that is not there. The replies are compared to
// each other rather than to a constant: any difference is the disclosure,
// whatever it is called.
func aclNoOracle(peer *imapClient, present, prefix string) error {
	absent := prefix + "SmokeNoSuchMailbox"

	for _, cmd := range []string{"SELECT %q", "STATUS %q (MESSAGES)", "GETACL %q", "MYRIGHTS %q",
		"LISTRIGHTS %q anyone", "SETACL %q anyone lr"} {
		_, errPresent := peer.cmd(fmt.Sprintf(cmd, present))
		_, errAbsent := peer.cmd(fmt.Sprintf(cmd, absent))

		name := strings.Fields(cmd)[0]
		if errPresent == nil {
			return fmt.Errorf("%s answered a peer with no rights for %q", name, present)
		}
		if errAbsent == nil {
			return fmt.Errorf("%s answered for %q, which does not exist", name, absent)
		}
		gotPresent := comparableRefusal(errPresent, present)
		gotAbsent := comparableRefusal(errAbsent, absent)
		if gotPresent != gotAbsent {
			return fmt.Errorf("%s lets a peer tell a mailbox it may not see from one that is not there:\n"+
				"  present: %v\n  absent:  %v", name, errPresent, errAbsent)
		}
	}
	return nil
}

// aclGrantsAreReachable asserts the other half: a grant must change what the
// peer sees. Without this the check would pass on a server that answers
// NONEXISTENT to every non-owner, which is what #1086 looked like.
func aclGrantsAreReachable(owner, peer *imapClient, folder, peerUser string) error {
	// Lookup only: the peer may now see that the mailbox exists, and the
	// refusal names the right it lacks instead of hiding the mailbox.
	if _, err := owner.cmd(fmt.Sprintf("SETACL %q %q l", folder, peerUser)); err != nil {
		return fmt.Errorf("SETACL l: %w", err)
	}
	if _, err := peer.cmd(fmt.Sprintf("SELECT %q", folder)); err == nil {
		return fmt.Errorf("SELECT succeeded for a peer holding only 'l'")
	} else if !strings.Contains(err.Error(), "NOPERM") {
		return fmt.Errorf("with 'l' granted, SELECT still hides the mailbox: %v — a grant that changes nothing "+
			"is the failure mode of #1086", err)
	}
	if _, err := peer.cmd(fmt.Sprintf("MYRIGHTS %q", folder)); err != nil {
		return fmt.Errorf("MYRIGHTS is not gated on the admin right and should answer with 'l': %w", err)
	}
	if _, err := peer.cmd(fmt.Sprintf("GETACL %q", folder)); err == nil {
		return fmt.Errorf("GETACL answered a peer without the 'a' right (RFC 4314 §4)")
	}

	// Read: the peer can open the mailbox.
	if _, err := owner.cmd(fmt.Sprintf("SETACL %q %q lr", folder, peerUser)); err != nil {
		return fmt.Errorf("SETACL lr: %w", err)
	}
	if _, err := peer.cmd(fmt.Sprintf("SELECT %q", folder)); err != nil {
		return fmt.Errorf("with 'lr' granted, SELECT still fails: %w", err)
	}

	// Admin: the peer can read the ACL.
	if _, err := owner.cmd(fmt.Sprintf("SETACL %q %q lra", folder, peerUser)); err != nil {
		return fmt.Errorf("SETACL lra: %w", err)
	}
	if _, err := peer.cmd(fmt.Sprintf("GETACL %q", folder)); err != nil {
		return fmt.Errorf("with 'a' granted, GETACL still fails: %w", err)
	}
	return nil
}

// comparableRefusal reduces a tagged reply to the part a client acts on.
//
// Two things differ between the two probes for reasons that are not
// disclosure, and both would make an identical server look like a leaking one:
//
//   - the command tag. cmd() returns the whole line, and the tag increments per
//     command, so the present probe is S0007 and the absent one S0008 --
//     different strings, same answer;
//   - the mailbox name, if a reply ever quotes it. Today none do, but the point
//     of comparing full text is to catch a rewording, and a rewording that
//     included the name would fire here for the wrong reason.
//
// Everything after that is kept and compared, because a difference in wording
// between the two answers is exactly what this is looking for.
func comparableRefusal(err error, folder string) string {
	if err == nil {
		return ""
	}
	line := err.Error()
	if _, rest, ok := strings.Cut(line, " "); ok {
		line = rest
	}
	return strings.ReplaceAll(line, folder, "<probe>")
}

// aclCleanupResult decides what a failed cleanup does to the check's result.
//
// It used to do nothing: the deferred delete reported through fmt.Printf, so a
// probe left behind exited 0 and the check accumulated mailboxes in the very
// namespace it exists to inspect. The likeliest cause is a bootstrap grant
// without 'x' (#1104), but any cause leaves the same litter, so the answer does
// not depend on knowing which.
//
// A check that already failed keeps its own error -- that one names the defect,
// this one only says the probe survived.
func aclCleanupResult(checkErr, cleanupErr error, folder, prefix string) error {
	if cleanupErr == nil {
		return checkErr
	}
	if checkErr == nil {
		return fmt.Errorf("cleanup %q: %w — the probe is still in %q", folder, cleanupErr, prefix)
	}
	return fmt.Errorf("%w (cleanup %q also failed: %v)", checkErr, folder, cleanupErr)
}
