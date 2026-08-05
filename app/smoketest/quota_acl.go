package main

import (
	"fmt"
	"strings"
)

// append sends an IMAP APPEND with a synchronizing literal and returns the
// tagged result line. A tagged NO/BAD comes back as an error (so an OVERQUOTA
// rejection is observable), OK returns nil.
func (c *imapClient) append(folder, body string) error {
	c.seq++
	tag := fmt.Sprintf("S%04d", c.seq)
	fmt.Fprintf(c.conn, "%s APPEND %q {%d}\r\n", tag, folder, len(body))
	// Server replies with a continuation "+ ..." before we send the literal.
	cont, err := c.r.ReadString('\n')
	if err != nil {
		return fmt.Errorf("append continuation: %w", err)
	}
	if !strings.HasPrefix(cont, "+") {
		return fmt.Errorf("expected continuation, got %q", strings.TrimSpace(cont))
	}
	fmt.Fprintf(c.conn, "%s\r\n", body)
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, tag+" OK") {
			return nil
		}
		if strings.HasPrefix(line, tag+" ") {
			return fmt.Errorf("%s", line)
		}
	}
}

// checkQuota verifies the IMAP QUOTA extension end-to-end (read-only): the
// capability is advertised and GETQUOTAROOT / GETQUOTA return a quota root with
// a STORAGE resource summed from the count backend.
func checkQuota(user, pass string) error {
	c, err := imapDial()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return fmt.Errorf("login %q: %w", user, err)
	}

	caps, err := c.cmd("CAPABILITY")
	if err != nil {
		return fmt.Errorf("capability: %w", err)
	}
	if !linesContain(caps, "QUOTA") {
		return fmt.Errorf("QUOTA not advertised in CAPABILITY")
	}

	roots, err := c.cmd("GETQUOTAROOT INBOX")
	if err != nil {
		return fmt.Errorf("getquotaroot: %w", err)
	}
	root := parseQuotaRoot(roots)
	if root == "" {
		return fmt.Errorf("no quota root in GETQUOTAROOT reply: %v", roots)
	}
	if !anyLineHas(roots, "* QUOTA ", "STORAGE") {
		return fmt.Errorf("no STORAGE resource in GETQUOTAROOT reply: %v", roots)
	}

	q, err := c.cmd(fmt.Sprintf("GETQUOTA %q", root))
	if err != nil {
		return fmt.Errorf("getquota %q: %w", root, err)
	}
	if !anyLineHas(q, "* QUOTA ", "STORAGE") {
		return fmt.Errorf("GETQUOTA %q missing STORAGE: %v", root, q)
	}
	return nil
}

// checkQuotaOver verifies enforcement: an APPEND by an over-quota user is
// rejected with a tagged NO [OVERQUOTA]. Requires a user provisioned at or over
// their storage quota.
func checkQuotaOver(user, pass string) error {
	c, err := imapDial()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return fmt.Errorf("login %q: %w", user, err)
	}
	body := "From: smoke@test\r\nSubject: over-quota probe\r\n\r\nprobe\r\n"
	err = c.append("INBOX", body)
	if err == nil {
		return fmt.Errorf("APPEND accepted for an over-quota user (expected OVERQUOTA)")
	}
	if !strings.Contains(strings.ToUpper(err.Error()), "OVERQUOTA") {
		return fmt.Errorf("APPEND rejected but not with OVERQUOTA: %v", err)
	}
	return nil
}

// checkACL verifies the IMAP ACL extension: the capability is advertised,
// MYRIGHTS returns rights on INBOX, and a SETACL/GETACL/DELETEACL round-trip on
// a throwaway folder works. The temp folder is always cleaned up.
func checkACL(user, pass string) error {
	c, err := imapDial()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return fmt.Errorf("login %q: %w", user, err)
	}

	caps, err := c.cmd("CAPABILITY")
	if err != nil {
		return fmt.Errorf("capability: %w", err)
	}
	if !linesContain(caps, "ACL") {
		return fmt.Errorf("ACL not advertised in CAPABILITY")
	}

	mr, err := c.cmd("MYRIGHTS INBOX")
	if err != nil {
		return fmt.Errorf("myrights INBOX: %w", err)
	}
	if !anyLineHas(mr, "* MYRIGHTS ", "") || parseMyRights(mr) == "" {
		return fmt.Errorf("MYRIGHTS INBOX returned no rights: %v", mr)
	}

	const folder = "SmokeACL"
	// Best-effort pre-clean (the folder is usually absent), then always clean
	// up on exit. The exit path reports: a cleanup that silently failed is how
	// the next run inherits state it did not create.
	_ = c.deleteFolder(folder)
	defer func() {
		if err := c.deleteFolder(folder); err != nil {
			fmt.Printf("  cleanup %q: %v\n", folder, err)
		}
	}()

	if _, err := c.cmd(fmt.Sprintf("CREATE %q", folder)); err != nil {
		return fmt.Errorf("create %q: %w", folder, err)
	}
	if _, err := c.cmd(fmt.Sprintf("SETACL %q anyone lr", folder)); err != nil {
		return fmt.Errorf("setacl: %w", err)
	}
	acl, err := c.cmd(fmt.Sprintf("GETACL %q", folder))
	if err != nil {
		return fmt.Errorf("getacl: %w", err)
	}
	if !anyLineHas(acl, "* ACL ", "anyone") {
		return fmt.Errorf("GETACL missing the anyone entry just set: %v", acl)
	}
	if _, err := c.cmd(fmt.Sprintf("DELETEACL %q anyone", folder)); err != nil {
		return fmt.Errorf("deleteacl: %w", err)
	}
	return nil
}

// ---- small response parsers ------------------------------------------------

func linesContain(lines []string, token string) bool {
	for _, l := range lines {
		if strings.Contains(l, token) {
			return true
		}
	}
	return false
}

// anyLineHas reports whether some line contains both prefix and needle
// (needle "" matches any line carrying the prefix).
func anyLineHas(lines []string, prefix, needle string) bool {
	for _, l := range lines {
		if strings.Contains(l, prefix) && (needle == "" || strings.Contains(l, needle)) {
			return true
		}
	}
	return false
}

// parseQuotaRoot extracts the root name (the last quoted token) from a
// `* QUOTAROOT INBOX "User quota"` line. The mailbox may be a bare atom
// (unquoted) while the root name is quoted, so the split can yield as few as
// three parts: ["* QUOTAROOT INBOX ", "User quota", ""].
func parseQuotaRoot(lines []string) string {
	for _, l := range lines {
		if !strings.HasPrefix(l, "* QUOTAROOT") {
			continue
		}
		parts := strings.Split(l, "\"")
		if len(parts) >= 2 {
			return parts[len(parts)-2] // last quoted token
		}
	}
	return ""
}

// parseMyRights returns the rights string from a `* MYRIGHTS "INBOX" lrsw...` line.
func parseMyRights(lines []string) string {
	for _, l := range lines {
		if !strings.HasPrefix(l, "* MYRIGHTS") {
			continue
		}
		fields := strings.Fields(l)
		if len(fields) >= 4 {
			return fields[len(fields)-1]
		}
	}
	return ""
}
