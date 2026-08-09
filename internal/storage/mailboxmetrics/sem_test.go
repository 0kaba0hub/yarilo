package mailboxmetrics_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Every driver has the same write semaphore, so every driver must time the
// wait for it. Instrumenting one and not the others tilts the comparison the
// metric exists for: the instrumented driver would subtract the queue from its
// own cost while the baseline keeps it inside.
func TestEveryDriverTimesTheWriteSemaphore(t *testing.T) {
	// One slot, so a save always takes it and the part is always recorded.
	drivers := map[string]mailbox.MailboxBackend{
		"maildir": maildir.New(maildir.WithMaxConcurrentWrites(1)),
		"sdbox":   dboxv2.New(dboxv2.WithMaxConcurrentWrites(1)),
		"mdbox":   mdbox.New(mdbox.WithMaxConcurrentWrites(1)),
	}
	for name, be := range drivers {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.MkdirAll(filepath.Join(home), 0o700); err != nil {
				t.Fatal(err)
			}
			box := be.OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home})
			t.Cleanup(func() { _ = box.Close() })
			if err := box.Init(); err != nil {
				t.Fatalf("init: %v", err)
			}

			before := semSamples(t, name)
			raw := "Subject: t\r\n\r\nbody\r\n"
			if _, _, _, err := box.Save("INBOX", strings.NewReader(raw), 1, int64(len(raw)), nil, [16]byte{}); err != nil {
				t.Fatalf("save: %v", err)
			}
			if got := semSamples(t, name); got == before {
				t.Errorf("%s does not time the wait for a write slot: the queue would land in the whole with nothing naming it", name)
			}
		})
	}
}

func semSamples(t *testing.T, driver string) uint64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var n uint64
	for _, f := range families {
		if f.GetName() != "mailbox_save_part_seconds" {
			continue
		}
		for _, m := range f.GetMetric() {
			if labelIs(m, "driver", driver) && labelIs(m, "part", "sem") {
				n += m.GetHistogram().GetSampleCount()
			}
		}
	}
	return n
}

func labelIs(m *dto.Metric, name, value string) bool {
	for _, l := range m.GetLabel() {
		if l.GetName() == name && l.GetValue() == value {
			return true
		}
	}
	return false
}
