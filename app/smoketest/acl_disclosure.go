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
func checkACLDisclosure(ownerUser, ownerPass, peerUser, peerPass, prefix string) error {
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
	defer func() {
		if err := owner.deleteFolder(folder); err != nil {
			fmt.Printf("  cleanup %q: %v\n", folder, err)
		}
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
		if errPresent.Error() != errAbsent.Error() {
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
