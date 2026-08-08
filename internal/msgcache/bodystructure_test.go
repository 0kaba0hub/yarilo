package msgcache

import (
	"reflect"
	"testing"

	imaplib "github.com/emersion/go-imap/v2"
)

// A structure deep and irregular enough that a flattening, a reordering or a
// dropped optional block all show: nested multipart, an embedded
// message/rfc822 carrying its own envelope AND its own structure, distinct
// NumLines per text part, and extension blocks present on some nodes and
// absent on their siblings.
func richBodyStructure() imaplib.BodyStructure {
	return &imaplib.BodyStructureMultiPart{
		Subtype: "mixed",
		Children: []imaplib.BodyStructure{
			&imaplib.BodyStructureMultiPart{
				Subtype: "alternative",
				Children: []imaplib.BodyStructure{
					&imaplib.BodyStructureSinglePart{
						Type: "text", Subtype: "plain",
						Params:   map[string]string{"charset": "utf-8"},
						Encoding: "7bit", Size: 111,
						Text: &imaplib.BodyStructureText{NumLines: 7},
					},
					&imaplib.BodyStructureSinglePart{
						Type: "text", Subtype: "html",
						Params:   map[string]string{"charset": "koi8-u"},
						Encoding: "quoted-printable", Size: 222,
						Text: &imaplib.BodyStructureText{NumLines: 13}, // != the sibling's
						Extended: &imaplib.BodyStructureSinglePartExt{
							Language: []string{"uk", "en"},
							Location: "cid:body",
						},
					},
				},
				Extended: &imaplib.BodyStructureMultiPartExt{
					Params:      map[string]string{"boundary": "alt-1"},
					Disposition: &imaplib.BodyStructureDisposition{Value: "inline"},
				},
			},
			&imaplib.BodyStructureSinglePart{
				Type: "message", Subtype: "rfc822",
				Encoding: "8bit", Size: 999,
				MessageRFC822: &imaplib.BodyStructureMessageRFC822{
					Envelope: &imaplib.Envelope{
						Subject:   "вкладений лист",
						From:      []imaplib.Address{{Name: "Аліса", Mailbox: "alice", Host: "example.com"}},
						MessageID: "<inner@x>",
					},
					BodyStructure: &imaplib.BodyStructureSinglePart{
						Type: "text", Subtype: "plain",
						Encoding: "base64", Size: 42,
						Text: &imaplib.BodyStructureText{NumLines: 3},
					},
					NumLines: 31,
				},
			},
			&imaplib.BodyStructureSinglePart{
				Type: "application", Subtype: "pdf",
				Params:   map[string]string{"name": "звіт.pdf"},
				ID:       "<att1>",
				Encoding: "base64", Size: 4096,
				Extended: &imaplib.BodyStructureSinglePartExt{
					Disposition: &imaplib.BodyStructureDisposition{
						Value:  "attachment",
						Params: map[string]string{"filename": "звіт.pdf"},
					},
				},
			},
		},
		Extended: &imaplib.BodyStructureMultiPartExt{
			Params: map[string]string{"boundary": "mix-9"},
		},
	}
}

func TestBodyStructureCodecRoundTrip(t *testing.T) {
	for i, in := range []imaplib.BodyStructure{
		richBodyStructure(),
		&imaplib.BodyStructureSinglePart{Type: "text", Subtype: "plain", Encoding: "7bit", Size: 1},
		&imaplib.BodyStructureMultiPart{Subtype: "mixed"}, // no children, no extension
	} {
		got, ok := decodeBodyStructure(encodeBodyStructure(in))
		if !ok {
			t.Fatalf("case %d: decode failed", i)
		}
		if !reflect.DeepEqual(got, in) {
			t.Errorf("case %d round-trip mismatch:\n got %#v\nwant %#v", i, got, in)
		}
	}
}

func TestBodyStructureCodecMalformedIsAMiss(t *testing.T) {
	good := encodeBodyStructure(richBodyStructure())
	for name, b := range map[string][]byte{
		"nil":             nil,
		"empty":           {},
		"unknown version": {99, bsKindSingle},
		"unknown kind":    {bsCodecVersion, 0x7e},
		"truncated":       good[:len(good)/2],
		"header only":     {bsCodecVersion},
	} {
		if bs, ok := decodeBodyStructure(b); ok {
			t.Errorf("%s decoded to %#v", name, bs)
		}
	}
}

// Depth is bounded: a hand-built chain of nested multiparts must be refused
// rather than recursed into, since the bytes come off disk.
func TestBodyStructureCodecDepthBounded(t *testing.T) {
	b := []byte{bsCodecVersion}
	for i := 0; i < 200; i++ {
		b = append(b, bsKindMulti)
		b = putStr(b, "mixed")
		b = putU32(b, 1) // one child follows
	}
	b = append(b, bsKindSingle)
	if _, ok := decodeBodyStructure(b); ok {
		t.Error("a 200-deep structure decoded; want the depth bound to refuse it")
	}
}
