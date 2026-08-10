package mailbox

import "testing"

// A userdb that names the driver as a field is not asking for anything new: the
// prefix of mail_location already decides which backend opens a user's mail.
// What matters is that the explicit field wins, and that it wins the same way
// as the other path fields do, so moving a userdb between the two spellings
// does not change the answer.
func TestApplyUserdbLetsTheExplicitDriverWin(t *testing.T) {
	ui := UserInfo{Username: "u@example.org", Home: "/home/u"}
	locErr, drvErr := ApplyUserdb(&ui, UserdbOverrides{
		MailLocation: "maildir:/srv/u/Maildir",
		Driver:       "mdbox",
	})
	if locErr != nil || drvErr != nil {
		t.Fatalf("ApplyUserdb: loc=%v drv=%v", locErr, drvErr)
	}
	if ui.Driver != "mdbox" {
		t.Errorf("driver is %q, want mdbox: the explicit field must beat the location prefix", ui.Driver)
	}
}

// The order is the whole point of having one entry point. Applying the location
// after the driver reverses the precedence silently, which is how the same
// userdb opened a mailbox as mdbox over IMAP and as maildir over POP3.
func TestApplyUserdbOrdersLocationThenDriver(t *testing.T) {
	tests := []struct {
		name       string
		overrides  UserdbOverrides
		wantDriver string
	}{
		{
			name:       "location alone names the driver",
			overrides:  UserdbOverrides{MailLocation: "maildir:/srv/u/Maildir"},
			wantDriver: "maildir",
		},
		{
			name:       "explicit field beats the prefix",
			overrides:  UserdbOverrides{MailLocation: "maildir:/srv/u/Maildir", Driver: "mdbox"},
			wantDriver: "mdbox",
		},
		{
			name:       "explicit field with no location at all",
			overrides:  UserdbOverrides{Driver: "sdbox"},
			wantDriver: "sdbox",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ui := UserInfo{Username: "u@example.org", Home: "/home/u"}
			if locErr, drvErr := ApplyUserdb(&ui, tc.overrides); locErr != nil || drvErr != nil {
				t.Fatalf("ApplyUserdb: loc=%v drv=%v", locErr, drvErr)
			}
			if ui.Driver != tc.wantDriver {
				t.Errorf("driver = %q, want %q", ui.Driver, tc.wantDriver)
			}
		})
	}
}

// A separate path field beats the modifier embedded in the location, and the
// templates in both are expanded — the same contract for every protocol,
// because there is now one place that implements it. (The template here is the
// short form; the expression form arrives with #1235.)
func TestApplyUserdbExpandsAndPrefersTheSeparateFields(t *testing.T) {
	ui := UserInfo{Username: "alice@example.org", Home: "/home/alice"}
	locErr, drvErr := ApplyUserdb(&ui, UserdbOverrides{
		IndexDir:     "~/index/%d",
		MailLocation: "maildir:~/Maildir:INDEX=/from-location:CONTROL=~/ctrl",
	})
	if locErr != nil || drvErr != nil {
		t.Fatalf("ApplyUserdb: loc=%v drv=%v", locErr, drvErr)
	}
	if want := "/home/alice/index/example.org"; ui.IndexDir != want {
		t.Errorf("IndexDir = %q, want %q", ui.IndexDir, want)
	}
	if want := "/home/alice/ctrl"; ui.ControlDir != want {
		t.Errorf("ControlDir = %q, want %q (from the location modifier)", ui.ControlDir, want)
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
		err := stampDriver(&ui, tc.in)
		if (err != nil) != tc.wantErr {
			t.Fatalf("stampDriver(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
		}
		if ui.Driver != tc.want {
			t.Errorf("stampDriver(%q) left driver %q, want %q", tc.in, ui.Driver, tc.want)
		}
	}
}

// A name we do not implement must not silently fall through to the driver the
// location named: reading a user's mailbox as a format it is not is worse than
// refusing the field.
func TestUnknownDriverLeavesTheStampedOneAlone(t *testing.T) {
	var ui UserInfo
	if err := stampLocation(&ui, "mdbox:/srv/u"); err != nil {
		t.Fatalf("stampLocation: %v", err)
	}
	if err := stampDriver(&ui, "obox"); err == nil {
		t.Fatal("an unimplemented driver name was accepted")
	}
	if ui.Driver != "mdbox" {
		t.Errorf("driver is %q after a refused field, want the one mail_location named", ui.Driver)
	}
}
