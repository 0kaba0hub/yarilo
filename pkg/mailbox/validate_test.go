package mailbox

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	rules := DefaultNameRules()
	cases := []struct {
		name      string
		folder    string
		nsSep     string
		layoutSep string
		wantErr   string // substring; empty means the name is accepted
	}{
		{"plain name", "Archive", "/", "/", ""},
		{"nested name", "Work/2026", "/", "/", ""},
		{"INBOX is an ordinary name here", "INBOX", "/", ".", ""},
		{"dotted name is not a traversal", "my.folder", ".", ".", ""},

		{"empty", "", "/", "/", "empty name"},
		{"parent segment", "..", "/", "/", `".." path segment`},
		{"parent segment nested", "a/../b", "/", "/", `".." path segment`},
		{"current segment", ".", "/", "/", `"." path segment`},
		{"adjacent separators", "a//b", "/", "/", "empty hierarchy segment"},
		{"absolute", "/etc/passwd", "/", "/", `begins with "/"`},
		{"home-relative", "~root/mail", "/", "/", `begins with "~"`},
		{"NUL", "a\x00b", "/", "/", "NUL"},

		// The separator rule is off in DefaultNameRules, so an ordinary dotted
		// name is accepted: it is retroactive against names a mailbox may
		// already hold.
		{"dotted name with the rule off", "example.com", "/", ".", ""},
		{"same separator is never a conflict", "a.b", ".", ".", ""},

		// Reserved segments are the names the *layout* owns, so the nested
		// layouts refuse their marker and nothing else: cur/new/tmp are
		// maildir's directories and do not occur here at all.
		{"the nested layout's own marker", "dbox-Mails", "/", "/", "storage layout owns"},
		{"and nested under a parent", "Work/dbox-Mails", "/", "/", "storage layout owns"},
		{"case-insensitive", "DBOX-mails", "/", "/", "storage layout owns"},
		{"only as a whole segment", "dbox-Mails-old", "/", "/", ""},
		{"a maildir directory name is not the nested layout's", "cur", "/", "/", ""},
		{"nor is it maildir's own problem, given the leading dot", "cur", "/", ".", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateName(tc.folder, tc.nsSep, tc.layoutSep, rules)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("ValidateName(%q) = %v, want accepted", tc.folder, err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("ValidateName(%q) accepted, want refused (%s)", tc.folder, tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("ValidateName(%q) = %v, want a refusal mentioning %q", tc.folder, err, tc.wantErr)
			case tc.wantErr != "" && !errors.Is(err, ErrInvalidFolderName):
				t.Errorf("ValidateName(%q) refusal does not carry ErrInvalidFolderName, so a protocol layer reports it as a server fault", tc.folder)
			}
		})
	}
}

// The rules are configurable, which means "off" has to actually be off --
// otherwise an operator who turns the check off still cannot use the names.
func TestValidateNameRulesAreConfigurable(t *testing.T) {
	off := NameRules{}
	for _, folder := range []string{"..", "a/../b", "cur", "/abs"} {
		if err := ValidateName(folder, "/", "/", off); err != nil {
			t.Errorf("with all rules off, ValidateName(%q) = %v, want accepted", folder, err)
		}
	}
	// Structural refusals stand regardless: an empty name is not a name, and a
	// NUL cannot reach a filesystem call whatever the policy says.
	for _, folder := range []string{"", "a\x00b"} {
		if err := ValidateName(folder, "/", "/", off); err == nil {
			t.Errorf("with all rules off, ValidateName(%q) was accepted", folder)
		}
	}

	fsOnly := NameRules{ValidateFSNames: true}
	if err := ValidateName("cur", "/", "/", fsOnly); err != nil {
		t.Errorf("with no reserved segments, ValidateName(\"cur\") = %v, want accepted", err)
	}
	if err := ValidateName("..", "/", "/", fsOnly); err == nil {
		t.Error("with fs names validated, ValidateName(\"..\") was accepted")
	}
}

func TestLayoutSeparator(t *testing.T) {
	for driver, want := range map[string]string{
		"maildir": ".",
		"":        ".",
		"mdbox":   "/",
		"sdbox":   "/",
		"dbox":    "/",
		"MDBOX":   "/",
	} {
		if got := LayoutSeparator(driver); got != want {
			t.Errorf("LayoutSeparator(%q) = %q, want %q", driver, got, want)
		}
	}
}

// The separator rule has to be reachable on its own: an operator turning it on
// must not have to accept anything else, and an operator leaving it off must
// keep the traversal checks. Folding the two together was the defect -- it left
// a deployment with dotted folder names no remedy except disabling the
// traversal refusals as well.
func TestRefuseLayoutSeparatorIsIndependent(t *testing.T) {
	sepOnly := NameRules{RefuseLayoutSeparator: true}
	if err := ValidateName("example.com", "/", ".", sepOnly); err == nil {
		t.Error("with the separator rule on, \"example.com\" was accepted on a maildir layout")
	}
	// A plain nested name, since anything with ".." also contains the layout
	// separator and would be refused by this rule for its own reason.
	if err := ValidateName("Work/2026", "/", ".", sepOnly); err != nil {
		t.Errorf("the separator rule alone refused %q: %v — it must not imply the path checks", "Work/2026", err)
	}
	if err := ValidateName("/etc/passwd", "/", "/", sepOnly); err != nil {
		t.Errorf("the separator rule alone refused %q: %v — the leading-slash check belongs to ValidateFSNames", "/etc/passwd", err)
	}

	fsOnly := NameRules{ValidateFSNames: true}
	if err := ValidateName("example.com", "/", ".", fsOnly); err != nil {
		t.Errorf("with the separator rule off, ValidateName(\"example.com\") = %v; an existing mailbox would become unselectable", err)
	}
	if err := ValidateName("../victim/Maildir", "/", ".", fsOnly); err == nil {
		t.Error("keeping dotted names cost the traversal check — that is the trade this key exists to avoid")
	}
}

// The reserved-segment rule applies only where a folder directory sits beside
// the layout's own. On maildir++ every folder carries a leading "." — ".new"
// cannot collide with the "new" the layout owns — so refusing the name there
// protected nothing and cost ordinary ones: "New" is a folder people make, and
// it was refused (#1091, found by an unrelated test that renamed to "New").
func TestReservedSegmentsApplyOnlyWhereTheyCanCollide(t *testing.T) {
	rules := DefaultNameRules()

	for _, name := range []string{"new", "cur", "New", "tmp"} {
		if err := ValidateName(name, "/", ".", rules); err != nil {
			t.Errorf("maildir layout: ValidateName(%q) = %v; the leading dot keeps it apart", name, err)
		}
	}
	// The nested layouts own their marker and nothing else: cur/new/tmp are
	// maildir's directories and never appear here, so refusing them protected
	// nothing on this side either.
	if err := ValidateName("dbox-Mails", "/", "/", rules); err == nil {
		t.Error("nested layout: the marker name was accepted; a folder there is ambiguous with it")
	}
	for _, name := range []string{"new", "cur", "tmp", "New"} {
		if err := ValidateName(name, "/", "/", rules); err != nil {
			t.Errorf("nested layout: ValidateName(%q) = %v; that name is maildir's, not this layout's", name, err)
		}
	}
	// The traversal rules are unaffected by the layout.
	for _, sep := range []string{".", "/"} {
		if err := ValidateName("../victim", "/", sep, rules); err == nil {
			t.Errorf("layout %q: a traversal name was accepted", sep)
		}
	}
}

// The validator and the escaper must reserve the same names, or one refuses a
// folder the other would have stored safely — the same "two sets, one set"
// requirement as the config validator and the namespace builder (#1087).
func TestValidatorAndEscaperReserveTheSameNames(t *testing.T) {
	rules := DefaultNameRules()
	candidates := []string{"cur", "new", "tmp", "New", "dbox-Mails", "DBOX-Mails", "Sales", "dbox-Mails-old"}

	for _, layoutSep := range []string{".", "/"} {
		for _, name := range candidates {
			refused := ValidateName(name, "/", layoutSep, rules) != nil
			escaped := EscapeStorageName(name, layoutSep, "^") != name

			if refused != escaped {
				t.Errorf("layout %q, name %q: validator refuses=%v, escaper escapes=%v — "+
					"one of them is protecting against a collision the other does not believe in",
					layoutSep, name, refused, escaped)
			}
		}
	}
}
