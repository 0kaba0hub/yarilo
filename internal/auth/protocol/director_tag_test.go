package protocol

import "testing"

// TestExtractDirectorTag mirrors TestExtractQuotaOverFlag (#746): a
// director_tag extra field, sourced from either a passdb column or a
// userdb-scoped field, must reach AuthResponse.DirectorTag with the
// userdb-scoped value taking priority.
func TestExtractDirectorTag(t *testing.T) {
	tests := []struct {
		name  string
		setup func() *Fields
		want  string
	}{
		{
			name:  "nil fields",
			setup: func() *Fields { return nil },
			want:  "",
		},
		{
			name:  "absent",
			setup: func() *Fields { return NewFields() },
			want:  "",
		},
		{
			name: "userdb-scoped",
			setup: func() *Fields {
				f := NewFields()
				f.Set("userdb_director_tag", "b")
				return f
			},
			want: "b",
		},
		{
			name: "bare (passdb column)",
			setup: func() *Fields {
				f := NewFields()
				f.Set("director_tag", "a")
				return f
			},
			want: "a",
		},
		{
			name: "userdb-scoped wins over bare",
			setup: func() *Fields {
				f := NewFields()
				f.Set("director_tag", "bare")
				f.Set("userdb_director_tag", "scoped")
				return f
			},
			want: "scoped",
		},
		{
			name: "empty value ignored",
			setup: func() *Fields {
				f := NewFields()
				f.Set("userdb_director_tag", "")
				return f
			},
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractDirectorTag(tc.setup()); got != tc.want {
				t.Fatalf("extractDirectorTag = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBuildAuthOKDirectorTag verifies director_tag round-trips onto the wire
// on the typed-fields path, and is absent when empty.
func TestBuildAuthOKDirectorTag(t *testing.T) {
	res := &AuthResponse{Username: "u@x", DirectorTag: "a"}
	reply := buildAuthOK("id1", res)
	if !containsToken(reply, "director_tag=a") {
		t.Fatalf("reply %q missing director_tag token", reply)
	}
	res2 := &AuthResponse{Username: "u@x"}
	if containsToken(buildAuthOK("id1", res2), "director_tag=") {
		t.Fatalf("empty director_tag must not be emitted")
	}
}

// TestBuildAuthOKDirectorTag_FieldsPath verifies the bag-driven wire path
// (the one a real SQL passdb with a director_tag column actually takes)
// emits the field automatically via WireForm, with no extract* call needed.
func TestBuildAuthOKDirectorTag_FieldsPath(t *testing.T) {
	f := NewFields()
	f.Set("user", "u@x")
	f.Set("director_tag", "b")
	res := &AuthResponse{Username: "u@x", Fields: f}
	reply := buildAuthOK("id1", res)
	if !containsToken(reply, "director_tag=b") {
		t.Fatalf("reply %q missing director_tag token from Fields bag", reply)
	}
}

// TestAssignField_DirectorTag proves the SQL userdb/passdb generic column
// forwarding path (AssignField) recognizes director_tag.
func TestAssignField_DirectorTag(t *testing.T) {
	info := &UserInfo{}
	if err := AssignField(info, "director_tag", "a"); err != nil {
		t.Fatalf("AssignField: %v", err)
	}
	if info.DirectorTag != "a" {
		t.Fatalf("DirectorTag = %q, want %q", info.DirectorTag, "a")
	}
}
