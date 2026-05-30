package varexpand

import "testing"

func TestExpand(t *testing.T) {
	cases := []struct {
		name, in, want string
		vars           Vars
	}{
		{"no verbs", "/var/lib/foo.dict", "/var/lib/foo.dict", Vars{}},
		{"username", "%u.dict", "alice@example.com.dict", Vars{Username: "alice@example.com"}},
		{"local part", "%n.dict", "alice.dict", Vars{Username: "alice@example.com"}},
		{"domain", "%d.dict", "example.com.dict", Vars{Username: "alice@example.com"}},
		{"no domain", "%d.dict", ".dict", Vars{Username: "alice"}},
		{"local part no @", "%n.dict", "alice.dict", Vars{Username: "alice"}},
		{"home", "%h/.metadata.dict", "/srv/home/alice/.metadata.dict", Vars{HomeDir: "/srv/home/alice"}},
		{"uid", "uid-%i.dict", "uid-1001.dict", Vars{UID: "1001"}},
		{"combined", "%h/dicts/%n/meta.dict", "/h/alice/dicts/alice/meta.dict", Vars{HomeDir: "/h/alice", Username: "alice@example.com"}},
		{"literal percent", "100%%", "100%", Vars{}},
		{"unknown verb passthrough", "%X/foo", "%X/foo", Vars{}},
		{"trailing percent", "foo%", "foo%", Vars{}},
		{"only percent", "%", "%", Vars{}},
		{"empty username variants", "%u-%n-%d-%h-%i", "----", Vars{}},
		{"escaped then verb", "%%h is %h", "%h is /home/x", Vars{HomeDir: "/home/x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Expand(tc.in, tc.vars); got != tc.want {
				t.Errorf("Expand(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
