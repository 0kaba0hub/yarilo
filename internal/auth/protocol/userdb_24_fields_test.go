package protocol

import "testing"

// A userdb query written for 2.4 answers with mail_index_path /
// mail_volatile_path, and the values it puts there are the ones INDEX= and
// VOLATILEDIR= carry in a mail_location. Both spellings have to land in the
// same place: accepting only ours means such a query looks answered while the
// redirection it asked for silently does not happen — the index keeps its fsync
// on the mail volume and nobody is told.
func TestUserdbAcceptsBothSpellingsOfThePathFields(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		get   func(*UserInfo) string
	}{
		{"our index field", "index_dir", "/idx/alice", func(u *UserInfo) string { return u.IndexDir }},
		{"2.4 index field", "mail_index_path", "/idx/alice", func(u *UserInfo) string { return u.IndexDir }},
		{"our volatile field", "volatile_dir", "/tmp/v/alice", func(u *UserInfo) string { return u.VolatileDir }},
		{"2.4 volatile field", "mail_volatile_path", "/tmp/v/alice", func(u *UserInfo) string { return u.VolatileDir }},
		{"our control field", "control_dir", "/ctl/alice", func(u *UserInfo) string { return u.ControlDir }},
		{"2.4 control field", "mail_control_path", "/ctl/alice", func(u *UserInfo) string { return u.ControlDir }},
		{"our alt field", "alt_dir", "/cold/alice", func(u *UserInfo) string { return u.AltDir }},
		{"2.4 alt field", "mail_alt_path", "/cold/alice", func(u *UserInfo) string { return u.AltDir }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var info UserInfo
			if err := AssignField(&info, tc.key, tc.value); err != nil {
				t.Fatalf("AssignField(%q): %v", tc.key, err)
			}
			if got := tc.get(&info); got != tc.value {
				t.Errorf("%s=%q landed as %q", tc.key, tc.value, got)
			}
		})
	}
}

// The mail_location modifiers are the third spelling of the same two values,
// and they must agree with the fields above rather than with each other only.
func TestMailLocationModifiersMatchTheFields(t *testing.T) {
	var viaMods UserInfo
	if err := AssignField(&viaMods, "mail_location",
		"maildir:/srv/alice/Maildir:INDEX=/idx/alice:VOLATILEDIR=/tmp/v/alice:CONTROL=/ctl/alice:ALT=/cold/alice"); err != nil {
		t.Fatalf("mail_location: %v", err)
	}

	var viaFields UserInfo
	for k, v := range map[string]string{
		"mail_index_path":    "/idx/alice",
		"mail_volatile_path": "/tmp/v/alice",
		"mail_control_path":  "/ctl/alice",
		"mail_alt_path":      "/cold/alice",
	} {
		if err := AssignField(&viaFields, k, v); err != nil {
			t.Fatalf("AssignField(%q): %v", k, err)
		}
	}

	if viaMods.IndexDir != viaFields.IndexDir {
		t.Errorf("INDEX= gave %q, mail_index_path gave %q", viaMods.IndexDir, viaFields.IndexDir)
	}
	if viaMods.VolatileDir != viaFields.VolatileDir {
		t.Errorf("VOLATILEDIR= gave %q, mail_volatile_path gave %q", viaMods.VolatileDir, viaFields.VolatileDir)
	}
	if viaMods.ControlDir != viaFields.ControlDir {
		t.Errorf("CONTROL= gave %q, mail_control_path gave %q", viaMods.ControlDir, viaFields.ControlDir)
	}
	if viaMods.AltDir != viaFields.AltDir {
		t.Errorf("ALT= gave %q, mail_alt_path gave %q", viaMods.AltDir, viaFields.AltDir)
	}
}

// An explicit field wins over a modifier in the location string, whichever
// spelling the field uses — otherwise moving a userdb query to the 2.4 names
// would quietly change which of the two sources wins.
func TestExplicitFieldWinsOverTheModifierInBothSpellings(t *testing.T) {
	for _, key := range []string{"index_dir", "mail_index_path"} {
		var info UserInfo
		if err := AssignField(&info, key, "/explicit"); err != nil {
			t.Fatalf("AssignField(%q): %v", key, err)
		}
		if err := AssignField(&info, "mail_location", "maildir:/srv/alice/Maildir:INDEX=/from-location"); err != nil {
			t.Fatalf("mail_location: %v", err)
		}
		if info.IndexDir != "/explicit" {
			t.Errorf("%s: modifier overrode the explicit field, got %q", key, info.IndexDir)
		}
	}
}

// mailbox_format was parsed and carried and then used by nothing: a userdb
// could say mdbox and get maildir, silently. It selects the driver now, and
// mail_driver is the same field under the reference's name for it.
func TestDriverFieldIsAcceptedUnderBothNames(t *testing.T) {
	for _, key := range []string{"mailbox_format", "mail_driver"} {
		var info UserInfo
		if err := AssignField(&info, key, "mdbox"); err != nil {
			t.Fatalf("AssignField(%q): %v", key, err)
		}
		if info.MailboxFormat != "mdbox" {
			t.Errorf("%s=mdbox landed as %q", key, info.MailboxFormat)
		}
	}
}
