package protocol

import "testing"

func TestAssignField_ControlDir(t *testing.T) {
	var info UserInfo
	if err := AssignField(&info, "control_dir", "/ctrl/alice"); err != nil {
		t.Fatalf("AssignField: %v", err)
	}
	if info.ControlDir != "/ctrl/alice" {
		t.Errorf("ControlDir = %q, want /ctrl/alice", info.ControlDir)
	}
}

func TestAssignField_MailLocationExtractsControl(t *testing.T) {
	var info UserInfo
	if err := AssignField(&info, "mail", "maildir:~/Maildir:CONTROL=/var/ctrl/%u"); err != nil {
		t.Fatalf("AssignField: %v", err)
	}
	if info.MailLocation != "maildir:~/Maildir:CONTROL=/var/ctrl/%u" {
		t.Errorf("MailLocation = %q", info.MailLocation)
	}
	if info.ControlDir != "/var/ctrl/%u" {
		t.Errorf("ControlDir = %q, want /var/ctrl/%%u", info.ControlDir)
	}
}

// Explicit control_dir= field must take priority over CONTROL= in mail=.
func TestAssignField_ControlDirPriority_ExplicitFirst(t *testing.T) {
	var info UserInfo
	// explicit field arrives first
	if err := AssignField(&info, "control_dir", "/explicit/ctrl"); err != nil {
		t.Fatalf("AssignField control_dir: %v", err)
	}
	// mail= carries a CONTROL= modifier — must be ignored
	if err := AssignField(&info, "mail", "maildir:~/Maildir:CONTROL=/from-mail/ctrl"); err != nil {
		t.Fatalf("AssignField mail: %v", err)
	}
	if info.ControlDir != "/explicit/ctrl" {
		t.Errorf("ControlDir = %q, want /explicit/ctrl (explicit wins)", info.ControlDir)
	}
}

// Priority holds even when mail= is processed before control_dir=.
func TestAssignField_ControlDirPriority_MailFirst(t *testing.T) {
	var info UserInfo
	// mail= processed first (sets ControlDir from modifier)
	if err := AssignField(&info, "mail", "maildir:~/Maildir:CONTROL=/from-mail/ctrl"); err != nil {
		t.Fatalf("AssignField mail: %v", err)
	}
	// explicit field overwrites
	if err := AssignField(&info, "control_dir", "/explicit/ctrl"); err != nil {
		t.Fatalf("AssignField control_dir: %v", err)
	}
	if info.ControlDir != "/explicit/ctrl" {
		t.Errorf("ControlDir = %q, want /explicit/ctrl (explicit wins)", info.ControlDir)
	}
}

// All four modifiers extracted together from mail=.
func TestAssignField_MailLocationExtractsAllMods(t *testing.T) {
	var info UserInfo
	loc := "maildir:~/Maildir:VOLATILEDIR=/tmp/v:INDEX=/idx/%u:CONTROL=/ctrl/%u:ALT=/cold/%u"
	if err := AssignField(&info, "mail", loc); err != nil {
		t.Fatalf("AssignField: %v", err)
	}
	if info.VolatileDir != "/tmp/v" {
		t.Errorf("VolatileDir = %q, want /tmp/v", info.VolatileDir)
	}
	if info.IndexDir != "/idx/%u" {
		t.Errorf("IndexDir = %q, want /idx/%%u", info.IndexDir)
	}
	if info.ControlDir != "/ctrl/%u" {
		t.Errorf("ControlDir = %q, want /ctrl/%%u", info.ControlDir)
	}
	if info.AltDir != "/cold/%u" {
		t.Errorf("AltDir = %q, want /cold/%%u", info.AltDir)
	}
}

func TestAssignField_AltDir(t *testing.T) {
	var info UserInfo
	if err := AssignField(&info, "alt_dir", "/mnt/cold/alice"); err != nil {
		t.Fatalf("AssignField: %v", err)
	}
	if info.AltDir != "/mnt/cold/alice" {
		t.Errorf("AltDir = %q, want /mnt/cold/alice", info.AltDir)
	}
}

func TestAssignField_MailLocationExtractsAlt(t *testing.T) {
	var info UserInfo
	if err := AssignField(&info, "mail", "maildir:~/Maildir:ALT=/mnt/cold/%u"); err != nil {
		t.Fatalf("AssignField: %v", err)
	}
	if info.AltDir != "/mnt/cold/%u" {
		t.Errorf("AltDir = %q, want /mnt/cold/%%u", info.AltDir)
	}
}

func TestAssignField_AltDirPriority(t *testing.T) {
	var info UserInfo
	// explicit alt_dir= arrives first
	if err := AssignField(&info, "alt_dir", "/explicit/cold"); err != nil {
		t.Fatalf("AssignField alt_dir: %v", err)
	}
	// mail= with ALT= modifier — must be ignored
	if err := AssignField(&info, "mail", "maildir:~/Maildir:ALT=/from-mail/cold"); err != nil {
		t.Fatalf("AssignField mail: %v", err)
	}
	if info.AltDir != "/explicit/cold" {
		t.Errorf("AltDir = %q, want /explicit/cold (explicit wins)", info.AltDir)
	}
}
