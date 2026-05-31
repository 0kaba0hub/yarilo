package mailindex

import (
	"bytes"
	"errors"
	"testing"
)

func TestHeaderRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		h    Header
	}{
		{
			name: "fresh from NewHeader",
			h:    NewHeader(1717185600),
		},
		{
			name: "after first sync",
			h: Header{
				MajorVersion:            MajorVersion,
				MinorVersion:            MinorVersion,
				BaseHeaderSize:          HeaderMinSize,
				HeaderSize:              HeaderMinSize,
				RecordSize:              RecordMinSize,
				CompatFlags:             CompatLittleEndian,
				IndexID:                 1717185600,
				Flags:                   HdrFlagHaveDirty,
				UIDValidity:             1717180000,
				NextUID:                 42,
				MessagesCount:           41,
				SeenMessagesCount:       30,
				DeletedMessagesCount:    1,
				FirstRecentUID:          37,
				FirstUnseenUIDLowwater:  31,
				FirstDeletedUIDLowwater: 12,
				LogFileSeq:              7,
				LogFileTailOffset:       1024,
				LogFileHeadOffset:       2048,
				Log2RotateTime:          1717100000,
				LastTempFileScan:        1717100100,
				DayStamp:                1717123456,
				DayFirstUID:             [8]uint32{42, 30, 18, 9, 4, 2, 1, 1},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := c.h.Encode(&buf); err != nil {
				t.Fatalf("encode: %v", err)
			}
			if buf.Len() != HeaderMinSize {
				t.Fatalf("encoded size %d, want %d", buf.Len(), HeaderMinSize)
			}
			got, err := DecodeHeader(&buf)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got != c.h {
				t.Errorf("round-trip mismatch:\n got:  %+v\n want: %+v", got, c.h)
			}
		})
	}
}

func TestHeaderRejectsWrongMajor(t *testing.T) {
	h := NewHeader(1)
	buf := h.EncodeBytes()
	buf[0] = MajorVersion + 1
	_, err := DecodeHeaderBytes(buf)
	if !errors.Is(err, ErrMajorMismatch) {
		t.Errorf("got %v, want ErrMajorMismatch", err)
	}
}

func TestHeaderRejectsBigEndian(t *testing.T) {
	h := NewHeader(1)
	buf := h.EncodeBytes()
	buf[12] = 0 // clear LITTLE_ENDIAN bit
	_, err := DecodeHeaderBytes(buf)
	if !errors.Is(err, ErrEndian) {
		t.Errorf("got %v, want ErrEndian", err)
	}
}

func TestHeaderRejectsShortBuffer(t *testing.T) {
	_, err := DecodeHeaderBytes(make([]byte, HeaderMinSize-1))
	if !errors.Is(err, ErrShortRead) {
		t.Errorf("got %v, want ErrShortRead", err)
	}
}

func TestNewHeaderDefaults(t *testing.T) {
	h := NewHeader(42)
	if h.MajorVersion != MajorVersion {
		t.Errorf("major=%d, want %d", h.MajorVersion, MajorVersion)
	}
	if h.MinorVersion != MinorVersion {
		t.Errorf("minor=%d, want %d", h.MinorVersion, MinorVersion)
	}
	if h.BaseHeaderSize != HeaderMinSize {
		t.Errorf("base header size=%d, want %d", h.BaseHeaderSize, HeaderMinSize)
	}
	if h.HeaderSize != HeaderMinSize {
		t.Errorf("header size=%d, want %d (no extensions)", h.HeaderSize, HeaderMinSize)
	}
	if h.RecordSize != RecordMinSize {
		t.Errorf("record size=%d, want %d (no extensions)", h.RecordSize, RecordMinSize)
	}
	if h.CompatFlags != CompatLittleEndian {
		t.Errorf("compat flags=0x%02x, want LITTLE_ENDIAN", h.CompatFlags)
	}
	if h.IndexID != 42 {
		t.Errorf("indexID=%d, want 42", h.IndexID)
	}
	if h.NextUID != 1 {
		t.Errorf("next_uid=%d, want 1 (per spec, monotonic from 1)", h.NextUID)
	}
}

func TestHeaderSizeAlign(t *testing.T) {
	cases := []struct {
		in, want uint32
	}{
		{0, 0},
		{1, 8},
		{7, 8},
		{8, 8},
		{9, 16},
		{15, 16},
		{16, 16},
		{17, 24},
		{120, 120},
		{121, 128},
	}
	for _, c := range cases {
		if got := HeaderSizeAlign(c.in); got != c.want {
			t.Errorf("HeaderSizeAlign(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestHeaderExactByteOffsets(t *testing.T) {
	// Pin every byte offset against the wire format spec.
	// This is the regression test that fires if anyone tweaks
	// encodeInto and silently shifts a field by a byte or two.
	h := Header{
		MajorVersion:            0xAA,
		MinorVersion:            0xBB,
		BaseHeaderSize:          0xCCDD,
		HeaderSize:              0x11223344,
		RecordSize:              0x55667788,
		CompatFlags:             CompatLittleEndian, // 0x01
		IndexID:                 0x10203040,
		Flags:                   0x00112233,
		UIDValidity:             0xAABBCCDD,
		NextUID:                 0x01020304,
		MessagesCount:           0x05060708,
		SeenMessagesCount:       0x090A0B0C,
		DeletedMessagesCount:    0x0D0E0F10,
		FirstRecentUID:          0x11121314,
		FirstUnseenUIDLowwater:  0x15161718,
		FirstDeletedUIDLowwater: 0x191A1B1C,
		LogFileSeq:              0x1D1E1F20,
		LogFileTailOffset:       0x21222324,
		LogFileHeadOffset:       0x25262728,
		Log2RotateTime:          0x292A2B2C,
		LastTempFileScan:        0x2D2E2F30,
		DayStamp:                0x31323334,
	}
	buf := h.EncodeBytes()
	// Spot-check critical offsets that are most likely to drift.
	if buf[0] != 0xAA || buf[1] != 0xBB {
		t.Errorf("major/minor at 0,1: 0x%02x 0x%02x", buf[0], buf[1])
	}
	if buf[12] != 0x01 {
		t.Errorf("compat_flags at 12: 0x%02x, want 0x01", buf[12])
	}
	if buf[13] != 0 || buf[14] != 0 || buf[15] != 0 {
		t.Errorf("unused[3] at 13..15 not zero")
	}
	// IndexID at 16 little-endian: 0x10203040 → 40 30 20 10
	if buf[16] != 0x40 || buf[17] != 0x30 || buf[18] != 0x20 || buf[19] != 0x10 {
		t.Errorf("indexid at 16..19: % x", buf[16:20])
	}
	// UIDValidity at 24: 0xAABBCCDD → DD CC BB AA
	if buf[24] != 0xDD || buf[27] != 0xAA {
		t.Errorf("uid_validity at 24..27: % x", buf[24:28])
	}
	// LogFileSeq at 60
	if buf[60] != 0x20 || buf[63] != 0x1D {
		t.Errorf("log_file_seq at 60..63: % x", buf[60:64])
	}
	// DayStamp at 84
	if buf[84] != 0x34 || buf[87] != 0x31 {
		t.Errorf("day_stamp at 84..87: % x", buf[84:88])
	}
}
