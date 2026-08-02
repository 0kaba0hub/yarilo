package decoder

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
)

func TestNewNoneReturnsNilDecoder(t *testing.T) {
	for _, driver := range []string{"", "none"} {
		d, err := New(config.FTSConfig{DecoderDriver: driver})
		if err != nil {
			t.Fatalf("driver %q: unexpected error: %v", driver, err)
		}
		if d != nil {
			t.Fatalf("driver %q: expected nil Decoder, got %v", driver, d)
		}
	}
}

func TestNewScriptRequiresAddr(t *testing.T) {
	if _, err := New(config.FTSConfig{DecoderDriver: "script"}); err == nil {
		t.Fatal("expected error for script driver with no addr")
	}
	d, err := New(config.FTSConfig{DecoderDriver: "script", DecoderScriptAddr: "unix:///tmp/x.sock"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d == nil {
		t.Fatal("expected non-nil Decoder")
	}
}

func TestNewTikaRequiresURL(t *testing.T) {
	if _, err := New(config.FTSConfig{DecoderDriver: "tika"}); err == nil {
		t.Fatal("expected error for tika driver with no url")
	}
	d, err := New(config.FTSConfig{DecoderDriver: "tika", DecoderTikaURL: "http://tika:9998"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d == nil {
		t.Fatal("expected non-nil Decoder")
	}
}

func TestNewUnknownDriverErrors(t *testing.T) {
	if _, err := New(config.FTSConfig{DecoderDriver: "bogus"}); err == nil {
		t.Fatal("expected error for unknown driver")
	}
}
