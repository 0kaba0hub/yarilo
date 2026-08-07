package mailbox_test

import (
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Every field a consumer would otherwise default has to travel, or the
// constructor is not the one place it claims to be. The ACL identity is the
// part that made this not shared: the delivery path carried Groups / ACLUser /
// ACLGroups and the other two did not, so an ACL entry naming a group resolved
// differently at delivery than at SELECT of the same shared mailbox (#1109).
func TestNamespaceUserInfoCarriesEveryFieldAConsumerWouldDefault(t *testing.T) {
	base := &mailbox.UserInfo{
		Username:          "u1@test",
		Home:              "/srv/u1",
		StorageEscapeChar: "^",
		SkipNFCNormalize:  true,
		Groups:            []string{"admins"},
		ACLUser:           "acl-u1",
		ACLGroups:         []string{"acl-admins"},
	}
	loc := mailbox.Location{
		Driver: "mdbox", Path: "/srv/public",
		IndexDir: "/idx", VolatileDir: "/vol", ControlDir: "/ctl", AltDir: "/alt",
	}

	ui, err := mailbox.NamespaceUserInfo(base, loc, ".")
	if err != nil {
		t.Fatalf("NamespaceUserInfo: %v", err)
	}

	// The paths come from the location...
	for _, tc := range []struct{ name, got, want string }{
		{"Home", ui.Home, "/srv/public"},
		{"MailPath", ui.MailPath, "/srv/public"},
		{"Driver", ui.Driver, "mdbox"},
		{"IndexDir", ui.IndexDir, "/idx"},
		{"VolatileDir", ui.VolatileDir, "/vol"},
		{"ControlDir", ui.ControlDir, "/ctl"},
		{"AltDir", ui.AltDir, "/alt"},
		{"Separator", ui.Separator, "."},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}

	// ...and the identity from the session, ACL identity included.
	if ui.Username != "u1@test" || ui.ACLUser != "acl-u1" {
		t.Errorf("identity = %q/%q, want u1@test/acl-u1", ui.Username, ui.ACLUser)
	}
	if len(ui.Groups) != 1 || ui.Groups[0] != "admins" {
		t.Errorf("Groups = %v — an ACL naming a group would resolve differently here", ui.Groups)
	}
	if len(ui.ACLGroups) != 1 || ui.ACLGroups[0] != "acl-admins" {
		t.Errorf("ACLGroups = %v", ui.ACLGroups)
	}
	if ui.StorageEscapeChar != "^" || !ui.SkipNFCNormalize {
		t.Errorf("storage-name form did not travel: %q %v", ui.StorageEscapeChar, ui.SkipNFCNormalize)
	}
}

// A namespace with no root path is an error, not a UserInfo with empty paths:
// that is the defaulting this constructor exists to prevent, and producing it
// from here would be the worst place to reintroduce it.
func TestNamespaceUserInfoRefusesAnEmptyRoot(t *testing.T) {
	ui, err := mailbox.NamespaceUserInfo(&mailbox.UserInfo{Username: "u1@test"}, mailbox.Location{Driver: "maildir"}, "/")
	if err == nil {
		t.Fatalf("an empty location produced %+v", ui)
	}
	if !strings.Contains(err.Error(), "root path") {
		t.Errorf("error %q does not say what is missing", err)
	}
}
