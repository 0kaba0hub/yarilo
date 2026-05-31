package mdbox

import (
	"strings"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

func TestScanIsExplicitlyNotImplemented(t *testing.T) {
	home := t.TempDir()
	mb := New()
	box := mb.OpenUser(&mailbox.UserInfo{Username: "alice", Home: home})
	_, err := box.Scan("INBOX")
	if err == nil {
		t.Fatal("scan returned nil error; want not-yet-implemented")
	}
	if !strings.Contains(err.Error(), "MDBOX-PROD-READY") {
		t.Errorf("err %q must mention MDBOX-PROD-READY phase so operators know where to look", err.Error())
	}
}
