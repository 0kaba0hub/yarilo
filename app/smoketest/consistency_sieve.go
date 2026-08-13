package main

import (
	"fmt"
	"strings"
	"time"
)

// Row: a script stored over ManageSieve takes effect on the next delivery. The
// two surfaces are the script store and the delivery path, and the agreement is
// that what one accepted is what the other applies — a ManageSieve check that
// only stores and reads back proves the store, not the effect, and the delivery
// checks never learn where the rules came from.
//
// The rule files into a folder named for this run, so the assertion is about
// this delivery and not about whatever a previous run left in INBOX.
const consistencySieveScript = "smoke-consistency"

func checkConsistencySieve(user, pass string) error {
	marker := consistencyMarker("sieve")
	folder := "consistency-" + marker

	if err := createFolder(user, pass, folder); err != nil {
		return fmt.Errorf("pre-create %s: %w", folder, err)
	}
	script := fmt.Sprintf("require [\"fileinto\"];\r\nif header :contains \"Subject\" %q { fileinto %q; }\r\n",
		marker, folder)
	if err := putAndActivateSieve(consistencySieveScript, script); err != nil {
		return fmt.Errorf("store the script over managesieve: %w", err)
	}
	defer cleanupConsistencySieve(user, pass, folder)

	if err := deliverConsistencyProbe(user, marker); err != nil {
		return err
	}
	return assertFiledInto(user, pass, folder, marker)
}

// putAndActivateSieve stores a script and makes it the active one. Storing
// without activating would leave the delivery running whatever was active
// before, and the row would report the old rules as this script's effect.
func putAndActivateSieve(name, body string) error {
	conn, err := msieveDial()
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()
	if err := msieveAuth(conn, *flagManageSieveUser, *flagManageSievePass); err != nil {
		return err
	}
	fmt.Fprintf(conn, "PUTSCRIPT %q {%d+}\r\n%s", name, len(body), body)
	line, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("PUTSCRIPT response: %w", err)
	}
	if !strings.HasPrefix(line, "OK") {
		return fmt.Errorf("PUTSCRIPT refused: %q", line)
	}
	fmt.Fprintf(conn, "SETACTIVE %q\r\n", name)
	line, err = readLine(conn)
	if err != nil {
		return fmt.Errorf("SETACTIVE response: %w", err)
	}
	if !strings.HasPrefix(line, "OK") {
		return fmt.Errorf("SETACTIVE refused: %q", line)
	}
	return nil
}

// assertFiledInto waits for the delivery to appear in the folder the script
// names, and refuses the case that looks like success from INBOX: the message
// delivered but filed nowhere.
func assertFiledInto(user, pass, folder, marker string) error {
	c, err := imapDial()
	if err != nil {
		return err
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	deadline := time.Now().Add(*flagTimeout * 3)
	for {
		if _, err := c.selectFolder(folder); err != nil {
			return fmt.Errorf("select %s: %w", folder, err)
		}
		uids, err := c.uidSearch("HEADER SUBJECT " + marker)
		if err != nil {
			return fmt.Errorf("uid search in %s: %w", folder, err)
		}
		if len(uids) == 1 {
			return nil
		}
		if time.Now().After(deadline) {
			where := "nowhere this row can see"
			if inbox, err := inboxHolds(c, marker); err == nil && inbox {
				where = "INBOX — the script was stored but did not apply"
			}
			return fmt.Errorf("the delivery is not in %s (%s)", folder, where)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func inboxHolds(c *imapClient, marker string) (bool, error) {
	if _, err := c.selectFolder("INBOX"); err != nil {
		return false, err
	}
	uids, err := c.uidSearch("HEADER SUBJECT " + marker)
	return len(uids) > 0, err
}

// cleanupConsistencySieve leaves the account as it was found: the script is
// deactivated and removed, and the folder this row created goes with it.
// Best-effort — a failure here must not turn a passing row red.
func cleanupConsistencySieve(user, pass, folder string) {
	if conn, err := msieveDial(); err == nil {
		defer conn.Close()
		if err := msieveAuth(conn, *flagManageSieveUser, *flagManageSievePass); err == nil {
			fmt.Fprintf(conn, "SETACTIVE \"\"\r\n")
			_, _ = readLine(conn)
			fmt.Fprintf(conn, "DELETESCRIPT %q\r\n", consistencySieveScript)
			_, _ = readLine(conn)
		}
	}
	if c, err := imapDial(); err == nil {
		defer c.close()
		if err := c.login(user, pass); err == nil {
			_ = c.deleteFolder(folder)
		}
	}
}
