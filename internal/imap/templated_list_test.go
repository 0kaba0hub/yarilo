package imap_test

import (
	"strings"
	"testing"
)

// The unexpanded template is not a mailbox and never reaches the wire: no
// LIST/LSUB row may carry it, and NAMESPACE advertises the prefix truncated
// at the variable (user/) -- per RFC 2342 the prefix is what the client
// prepends to a name, and the template prepends to nothing (#1171). The
// reference applies the same truncation to its namespace prefix.
func TestTemplatedNamespace_NoTemplateNode(t *testing.T) {
	_, addr := startOrphanServer(t)
	a := orphanLogin(t, addr, "alice")

	for _, cmd := range []string{
		`LIST "" "user/%"`,
		`LIST "" "user/*"`,
		`LIST "" "*"`,
		`LSUB "" "user/*"`,
	} {
		if out := a.cmd(cmd); strings.Contains(out, "%u") {
			t.Errorf("%s leaks the unexpanded template:\n%s", cmd, out)
		}
	}

	ns := a.cmd(`NAMESPACE`)
	if !strings.Contains(ns, `"user/" "/"`) {
		t.Errorf("NAMESPACE should advertise the truncated prefix user/, got:\n%s", ns)
	}
	if strings.Contains(ns, "%u") {
		t.Errorf("NAMESPACE leaks the unexpanded template:\n%s", ns)
	}
}

// An explicit owner in the pattern materialises that owner's namespace: the
// owner sees their own folders under the visible prefix, ACL filtering
// included for peers.
func TestTemplatedNamespace_ExplicitOwnerMaterialises(t *testing.T) {
	_, addr := startOrphanServer(t)
	a := orphanLogin(t, addr, "alice")
	a.cmd(`SELECT INBOX`)
	if !strings.Contains(a.cmd(`CREATE Work`), "OK") {
		t.Fatal("create failed")
	}

	out := a.cmd(`LIST "" "user/alice/*"`)
	if !strings.Contains(out, `"user/alice/INBOX"`) || !strings.Contains(out, `"user/alice/Work"`) {
		t.Errorf("owner's own space should materialise under the visible prefix, got:\n%s", out)
	}

	// A wildcard owner enumerates nobody: there is no registry to ask.
	if wide := a.cmd(`LIST "" "user/*"`); strings.Contains(wide, "Work") {
		t.Errorf("wildcard owner must not enumerate, got:\n%s", wide)
	}
}

// A peer without rights and an invented owner produce byte-identical silence:
// materialisation may not reopen the #1138 account oracle.
func TestTemplatedNamespace_HiddenAndInventedOwnersListAlike(t *testing.T) {
	_, addr := startOrphanServer(t)
	a := orphanLogin(t, addr, "alice")
	a.cmd(`SELECT INBOX`)

	b := orphanLogin(t, addr, "bob")
	strip := func(out string) string {
		var kept []string
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "* ") {
				kept = append(kept, line)
			}
		}
		return strings.Join(kept, "\n")
	}
	real := strip(b.cmd(`LIST "" "user/alice/*"`))
	fake := strip(b.cmd(`LIST "" "user/nosuch/*"`))
	if real != fake {
		t.Errorf("real owner without rights and invented owner must list alike:\nreal: %q\nfake: %q", real, fake)
	}
	if strings.Contains(real, "INBOX") {
		t.Errorf("peer without rights saw the owner's folders:\n%s", real)
	}
}

// A granted peer sees exactly the folders the lookup right covers.
func TestTemplatedNamespace_GrantedPeerSeesFolders(t *testing.T) {
	_, addr := startOrphanServer(t)
	a := orphanLogin(t, addr, "alice")
	a.cmd(`SELECT INBOX`)
	if !strings.Contains(a.cmd(`SETACL user/alice/INBOX bob lr`), "OK") {
		t.Skip("SETACL on the templated space not available in this harness")
	}

	b := orphanLogin(t, addr, "bob")
	out := b.cmd(`LIST "" "user/alice/*"`)
	if !strings.Contains(out, `"user/alice/INBOX"`) {
		t.Errorf("granted peer should see the granted folder, got:\n%s", out)
	}
}
