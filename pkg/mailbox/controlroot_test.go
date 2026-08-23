package mailbox

import "testing"

// The rule itself, and the precedence that matters: an explicit CONTROL=
// overrides the mail path, which overrides the home. Every service that writes
// a per-account control file has to agree, or one writes where another does
// not look -- and the symptom is not a crash but a subscription list that is
// full in one protocol and empty in another (#1437).
func TestControlRoot(t *testing.T) {
	tests := []struct {
		name string
		ui   *UserInfo
		want string
	}{
		{
			name: "explicit control dir wins over everything",
			ui:   &UserInfo{Home: "/home/u", MailPath: "/mail/u", ControlDir: "/ctrl/u"},
			want: "/ctrl/u",
		},
		{
			// The row that separates the rule from "just use the home": with a
			// mail path and no CONTROL=, control files follow the mail.
			name: "mail path wins over the home",
			ui:   &UserInfo{Home: "/home/u", MailPath: "/mail/u"},
			want: "/mail/u",
		},
		{
			name: "home is the fallback",
			ui:   &UserInfo{Home: "/home/u"},
			want: "/home/u",
		},
		{
			name: "no user at all",
			ui:   nil,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ControlRoot(tt.ui); got != tt.want {
				t.Errorf("ControlRoot = %q, want %q", got, tt.want)
			}
		})
	}
}
