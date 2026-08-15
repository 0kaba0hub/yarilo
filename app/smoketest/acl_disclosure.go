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

	// A fresh peer session, dialed on demand. The ACL cache (acl_cache_ttl)
	// holds a session's pre-grant answer for up to the TTL, so a peer opened
	// before a grant keeps the stale view; reconnecting after each grant is how
	// a real client sees it, and it asserts the thing that matters -- a grant
	// is visible to a new session -- without waiting the TTL out (#1121).
	dialPeer := func() (*imapClient, error) {
		c, derr := imapDial()
		if derr != nil {
			return nil, fmt.Errorf("dial (peer): %w", derr)
		}
		if lerr := c.login(peerUser, peerPass); lerr != nil {
			c.close()
			return nil, fmt.Errorf("login %q: %w", peerUser, lerr)
		}
		return c, nil
	}

	peer, err := dialPeer()
	if err != nil {
		return err
	}
	defer func() { peer.close() }()

	if err := aclPeerIsRightless(peer, folder, prefix, peerUser); err != nil {
		return err
	}
	if err := aclNoOracle(peer, folder, prefix); err != nil {
		return err
	}
	if err := aclGrantsAreReachable(owner, &peer, folder, peerUser, dialPeer); err != nil {
		return err
	}
	return nil
}

// aclPeerIsRightless establishes the premise the rest of this check rests on:
// the peer holds no rights on the probe mailbox, by any route -- its own entry,
// an ancestor's, or the namespace root's.
//
// Without this the check still answers, and the answer looks like a finding.
// A peer that inherits read from the namespace root SELECTs the mailbox
// lawfully, and the row reports "SELECT answered a peer with no rights",
// naming a hole that is not there (#1317: an evening spent on it). Inherited
// rights are invisible in the mailbox's own ACL, which is where anyone
// checks -- so the premise cannot be established by reading configuration, only
// by asking the deployment.
//
// A peer with rights is not a failure of the product. It is this check being
// pointed at an account it cannot measure anything with, which is why it skips.
func aclPeerIsRightless(peer *imapClient, folder, prefix, peerUser string) error {
	if _, err := peer.cmd(fmt.Sprintf("MYRIGHTS %q", folder)); err == nil {
		return unmeasurable("-acl-peer-user %q already holds rights on %q "+
			"(its own, an ancestor's, or the %q root's) -- name an account with no grant anywhere in this namespace",
			peerUser, folder, prefix)
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
func aclGrantsAreReachable(owner *imapClient, peer **imapClient, folder, peerUser string, dialPeer func() (*imapClient, error)) error {
	// Grant, then reconnect the peer so it sees the grant now rather than after
	// acl_cache_ttl. The pre-grant session would hold its cached refusal for up
	// to the TTL, which is what made this check fail against a correct server
	// while the same ladder passed by hand -- each manual probe was a new
	// connection (#1121).
	grant := func(rights string) error {
		if _, err := owner.cmd(fmt.Sprintf("SETACL %q %q %s", folder, peerUser, rights)); err != nil {
			return fmt.Errorf("SETACL %s: %w", rights, err)
		}
		(*peer).close()
		next, err := dialPeer()
		if err != nil {
			return err
		}
		*peer = next
		return nil
	}

	// Lookup only: the peer may now see that the mailbox exists, and the
	// refusal names the right it lacks instead of hiding the mailbox.
	if err := grant("l"); err != nil {
		return err
	}
	if _, err := (*peer).cmd(fmt.Sprintf("SELECT %q", folder)); err == nil {
		return fmt.Errorf("SELECT succeeded for a peer holding only 'l'")
	} else if !strings.Contains(err.Error(), "NOPERM") {
		return fmt.Errorf("with 'l' granted, SELECT does not name the missing right: %v — expected NOPERM, "+
			"so the grant is applied and the peer lacks only 'r'", err)
	}
	if _, err := (*peer).cmd(fmt.Sprintf("MYRIGHTS %q", folder)); err != nil {
		return fmt.Errorf("MYRIGHTS is not gated on the admin right and should answer with 'l': %w", err)
	}
	if _, err := (*peer).cmd(fmt.Sprintf("GETACL %q", folder)); err == nil {
		return fmt.Errorf("GETACL answered a peer without the 'a' right (RFC 4314 §4)")
	}

	// Read: the peer can open the mailbox.
	if err := grant("lr"); err != nil {
		return err
	}
	if _, err := (*peer).cmd(fmt.Sprintf("SELECT %q", folder)); err != nil {
		return fmt.Errorf("with 'lr' granted, SELECT still fails: %w", err)
	}

	// Admin: the peer can read the ACL.
	if err := grant("lra"); err != nil {
		return err
	}
	if _, err := (*peer).cmd(fmt.Sprintf("GETACL %q", folder)); err != nil {
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
