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

// All three modifiers extracted together from mail=.
func TestAssignField_MailLocationExtractsAllMods(t *testing.T) {
	var info UserInfo
	loc := "maildir:~/Maildir:VOLATILEDIR=/tmp/v:INDEX=/idx/%u:CONTROL=/ctrl/%u"
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
}
