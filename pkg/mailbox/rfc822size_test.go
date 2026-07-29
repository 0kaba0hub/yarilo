package mailbox

import "testing"

// TestRFC822Size pins the rule the FETCH and SEARCH paths depend on (#892):
// report the virtual (CRLF) size, and fall back to the physical size only when
// the backend recorded no virtual one.
func TestRFC822Size(t *testing.T) {
	tests := []struct {
		name  string
		size  uint32
		vsize uint32
		want  uint32
	}{
		{
			// The reported bug: a copy stored with bare LF is 25 octets shorter on
			// disk than what the server transmits, so the physical size would
			// under-report by exactly the number of bare LFs.
			name: "stored with bare LF", size: 356, vsize: 381, want: 381,
		},
		{
			// Stored with CRLF: both sizes agree, which is why the bug was
			// invisible for messages that arrived over the wire unmodified.
			name: "stored with CRLF", size: 381, vsize: 381, want: 381,
		},
		{
			name: "no virtual size recorded", size: 356, vsize: 0, want: 356,
		},
		{
			name: "both zero", size: 0, vsize: 0, want: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := &MessageMeta{Size: tc.size, VSize: tc.vsize}
			if got := m.RFC822Size(); got != tc.want {
				t.Fatalf("MessageMeta.RFC822Size() = %d, want %d", got, tc.want)
			}
			r := &ScanRecord{Size: tc.size, VSize: tc.vsize}
			if got := r.RFC822Size(); got != tc.want {
				t.Fatalf("ScanRecord.RFC822Size() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRFC822SizeNilReceiver(t *testing.T) {
	var m *MessageMeta
	if got := m.RFC822Size(); got != 0 {
		t.Fatalf("nil MessageMeta = %d, want 0", got)
	}
	var r *ScanRecord
	if got := r.RFC822Size(); got != 0 {
		t.Fatalf("nil ScanRecord = %d, want 0", got)
	}
}
