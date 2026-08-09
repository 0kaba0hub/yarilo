package mailbox

import "testing"

// A userdb that names the driver as a field is not asking for anything new: the
// prefix of mail_location already decides which backend opens a user's mail.
// What matters is that the explicit field wins, and that it wins the same way
// as the other path fields do, so moving a userdb between the two spellings
// does not change the answer.
func TestStampDriverOverridesTheLocationPrefix(t *testing.T) {
	var ui UserInfo
	if err := StampLocation(&ui, "maildir:/srv/u/Maildir"); err != nil {
		t.Fatalf("StampLocation: %v", err)
	}
	if ui.Driver != "maildir" {
		t.Fatalf("location stamped %q, want maildir", ui.Driver)
	}
	if err := StampDriver(&ui, "mdbox"); err != nil {
		t.Fatalf("StampDriver: %v", err)
	}
	if ui.Driver != "mdbox" {
		t.Errorf("driver is %q after an explicit field, want mdbox", ui.Driver)
	}
}

func TestStampDriverNames(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"maildir", "maildir", false},
		{"mdbox", "mdbox", false},
		{"sdbox", "sdbox", false},
		{"dbox", "sdbox", false}, // the alias resolves to the same backend
		{"MDBOX", "mdbox", false},
		{" mdbox ", "mdbox", false},
		{"", "", false},
		{"obox", "", true},
		{"maildir:/srv/u", "", true}, // a whole location is not a driver name
	}
	for _, tc := range tests {
		ui := UserInfo{Driver: ""}
		err := StampDriver(&ui, tc.in)
		if (err != nil) != tc.wantErr {
			t.Fatalf("StampDriver(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
		}
		if ui.Driver != tc.want {
			t.Errorf("StampDriver(%q) left driver %q, want %q", tc.in, ui.Driver, tc.want)
		}
	}
}

// A name we do not implement must not silently fall through to the driver the
// location named: reading a user's mailbox as a format it is not is worse than
// refusing the field.
func TestUnknownDriverLeavesTheStampedOneAlone(t *testing.T) {
	var ui UserInfo
	if err := StampLocation(&ui, "mdbox:/srv/u"); err != nil {
		t.Fatalf("StampLocation: %v", err)
	}
	if err := StampDriver(&ui, "obox"); err == nil {
		t.Fatal("an unimplemented driver name was accepted")
	}
	if ui.Driver != "mdbox" {
		t.Errorf("driver is %q after a refused field, want the one mail_location named", ui.Driver)
	}
}
