package imapthread

import (
	"fmt"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
)

// singletons is the shape the field fixture actually has, and the worst input
// gatherBySubject can get: every message a conversation of one, every subject
// distinct, no References anywhere.
//
// The first version of this benchmark did not measure it -- it chained every
// fifth message and reused subjects, which is a cheaper case, and reported
// 8.3ms where the field spends ~56ms. A benchmark on a shape the corpus does
// not have measures something nobody runs.
func singletons(n int) []Message {
	msgs := make([]Message, 0, n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, Message{
			Num:       uint32(i + 1),
			MessageID: fmt.Sprintf("<msg-%d@example.com>", i),
			Subject:   fmt.Sprintf("Unique subject %d about %d", i, i*7),
			Sent:      time.Date(2026, 3, 1, 0, 0, i, 0, time.UTC),
			Arrival:   time.Date(2026, 3, 1, 0, 0, i, 0, time.UTC),
			Size:      int64(1000 + i),
		})
	}
	return msgs
}

func BenchmarkReferencesSingletons10k(b *testing.B) {
	msgs := singletons(10000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := References(msgs); len(got) != 10000 {
			b.Fatalf("threads = %d, want 10000 -- the corpus is not singletons", len(got))
		}
	}
}

// tenThousand builds a mailbox-shaped corpus: mostly unrelated mail with a
// scattering of conversations, which is what a real account looks like and
// what the sandbox fixture holds.
func corpus(n int) []Message {
	msgs := make([]Message, 0, n)
	for i := 0; i < n; i++ {
		m := Message{
			Num:       uint32(i + 1),
			MessageID: fmt.Sprintf("<msg-%d@example.com>", i),
			Subject:   fmt.Sprintf("Subject number %d", i),
			Sent:      time.Date(2026, 3, 1, 0, 0, i, 0, time.UTC),
			Arrival:   time.Date(2026, 3, 1, 0, 0, i, 0, time.UTC),
			Size:      int64(1000 + i),
		}
		// Every fifth message answers the one before it, so the graph has real
		// edges rather than ten thousand singletons.
		if i%5 != 0 {
			m.References = []string{fmt.Sprintf("<msg-%d@example.com>", i-1)}
			m.Subject = "Re: " + msgs[i-1].Subject
		}
		msgs = append(msgs, m)
	}
	return msgs
}

// The question this exists to answer: of the ~320ms a THREAD takes on 10 442
// messages, how much is the threading itself?
//
// The layer table in #1461 attributed ~180ms to grouping and the reference
// graph, but that figure came from subtracting one command's time from
// another's -- arithmetic that assigns cost without measuring it. This
// measures it.
func BenchmarkReferences10k(b *testing.B) {
	msgs := corpus(10000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := References(msgs); len(got) == 0 {
			b.Fatal("no threads")
		}
	}
}

func BenchmarkOrderedSubject10k(b *testing.B) {
	msgs := corpus(10000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := OrderedSubject(msgs); len(got) == 0 {
			b.Fatal("no threads")
		}
	}
}

func BenchmarkOrderedSubjectSingletons10k(b *testing.B) {
	msgs := singletons(10000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := OrderedSubject(msgs); len(got) == 0 {
			b.Fatal("no threads")
		}
	}
}

func BenchmarkSortSubjectSingletons10k(b *testing.B) {
	msgs := singletons(10000)
	criteria := []imaplib.SortCriterion{{Key: imaplib.SortKeySubject}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Sort(msgs, criteria)
	}
}

func BenchmarkSortDateSingletons10k(b *testing.B) {
	msgs := singletons(10000)
	criteria := []imaplib.SortCriterion{{Key: imaplib.SortKeyDate}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Sort(msgs, criteria)
	}
}
