package jmap

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The claim under test (#1026): JMAP read latency is driven by how many folders
// an account has rather than by how much the client asked for. Every request is
// said to walk the whole folder set — Email/query building its scope, Email/get
// resolving ids, Mailbox/get reading counters.
//
// That claim came from reading the code, and reading the code produced a lever
// that was wrong by a factor of five once already this month (#1015). So it is
// measured here instead: the answer size is pinned and only the folder count
// moves. If cost tracks folders, these numbers grow with the sub-benchmark name
// while the reply stays the same size.
//
//	go test -tags '' -run xxx -bench FolderScaling -benchtime 20x ./internal/jmap/
func benchServer(b *testing.B, folders, messagesPerFolder int) *Server {
	b.Helper()
	// The index logs a line per folder it creates, which at fifty folders
	// buries the numbers this exists to produce.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	home := b.TempDir()
	info := &mailbox.UserInfo{Username: testUser, Home: home, Separator: "/"}
	locker := &testLocker{}

	mb := maildir.New()
	box := mb.OpenUser(info)
	b.Cleanup(func() { box.Close() }) //nolint:errcheck
	if err := box.Init(); err != nil {
		b.Fatalf("init: %v", err)
	}

	names := []string{"INBOX"}
	for i := 1; i < folders; i++ {
		name := fmt.Sprintf("Folder%03d", i)
		if err := box.Create(name); err != nil {
			b.Fatalf("create %s: %v", name, err)
		}
		names = append(names, name)
	}

	idx := file.New(file.WithLocker(locker))
	ui := idx.OpenUser(info)
	body := "Subject: bench\r\nFrom: a@example.com\r\n\r\nbody\r\n"
	for _, name := range names {
		f, err := ui.OpenFolder(name, 0)
		if err != nil {
			b.Fatalf("open %s: %v", name, err)
		}
		for uid := 1; uid <= messagesPerFolder; uid++ {
			fname, vsize, guid, serr := box.Save(name, strings.NewReader(body), uint32(uid), int64(len(body)), nil, [16]byte{})
			if serr != nil {
				b.Fatalf("save: %v", serr)
			}
			if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{
				UID: uint32(uid), Filename: fname, Size: uint32(len(body)), VSize: vsize,
				GUID: guid, InternalDate: time.Now(),
			}); err != nil {
				b.Fatalf("append: %v", err)
			}
		}
	}
	if err := ui.Close(); err != nil {
		b.Fatalf("index close: %v", err)
	}

	return New(Options{
		Trust:  ResolveTrust(false, true, []*net.IPNet{mustCIDRB(b, "192.0.2.0/24")}),
		Limits: testLimits(),
		Storage: &Storage{
			Mailbox:     maildir.New(),
			Index:       file.New(file.WithLocker(locker)),
			ResolveUser: func(string) (*mailbox.UserInfo, error) { return info, nil },
			Locker:      locker,
		},
	})
}

func mustCIDRB(b *testing.B, s string) *net.IPNet {
	b.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		b.Fatalf("cidr %s: %v", s, err)
	}
	return n
}

// post issues one API batch and fails the benchmark if the server did not
// answer it — a benchmark timing an error path measures nothing.
func post(b *testing.B, s *Server, body string) {
	b.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, apiRequest(body))
	if w.Code != 200 {
		b.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"error"`) {
		b.Fatalf("method error: %s", w.Body.String())
	}
}

var folderCounts = []int{1, 5, 20, 50}

// Mailbox/get asks for every mailbox, so its answer does grow with the folder
// count. It is here as the control: whatever Email/query does, this is what
// "cost proportional to folders" looks like when the answer is proportional too.
func BenchmarkFolderScalingMailboxGet(b *testing.B) {
	for _, folders := range folderCounts {
		b.Run(fmt.Sprintf("folders=%d", folders), func(b *testing.B) {
			s := benchServer(b, folders, 5)
			const body = `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
				"methodCalls":[["Mailbox/get",{"accountId":"u1@example.com","ids":null},"c0"]]}`
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				post(b, s, body)
			}
		})
	}
}

// The one that matters. The client asks for ten messages from one mailbox and
// the answer is the same size at every folder count, so any growth here is work
// the client did not ask for.
func BenchmarkFolderScalingEmailQueryGet(b *testing.B) {
	for _, folders := range folderCounts {
		b.Run(fmt.Sprintf("folders=%d", folders), func(b *testing.B) {
			s := benchServer(b, folders, 5)
			const body = `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
				"methodCalls":[
					["Email/query",{"accountId":"u1@example.com","limit":10,
						"sort":[{"property":"receivedAt","isAscending":false}]},"q0"],
					["Email/get",{"accountId":"u1@example.com",
						"#ids":{"resultOf":"q0","name":"Email/query","path":"/ids"},
						"properties":["id","subject","receivedAt"]},"g0"]]}`
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				post(b, s, body)
			}
		})
	}
}

// A client asking for one mailbox should pay for one mailbox. It does not:
// queryScope opens every folder and applies the inMailbox filter afterwards, so
// the filter narrows the answer without narrowing the work.
//
// It cannot simply be reordered. The JMAP mailbox id is the folder's GUID, and
// the GUID lives inside the folder's own index — so "which folder is mailbox X"
// is a question that can only be answered by opening folders. That is the thing
// missing at the account level, and this benchmark is what would show it fixed.
func BenchmarkFolderScalingEmailQueryInOneMailbox(b *testing.B) {
	for _, folders := range folderCounts {
		b.Run(fmt.Sprintf("folders=%d", folders), func(b *testing.B) {
			s := benchServer(b, folders, 5)
			// Resolve INBOX's id the way a client does, from Mailbox/get.
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, apiRequest(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
				"methodCalls":[["Mailbox/get",{"accountId":"u1@example.com","ids":null},"c0"]]}`))
			id := firstMailboxID(b, w.Body.String())

			body := fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
				"methodCalls":[
					["Email/query",{"accountId":"u1@example.com","limit":10,
						"filter":{"inMailbox":%q},
						"sort":[{"property":"receivedAt","isAscending":false}]},"q0"]]}`, id)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				post(b, s, body)
			}
		})
	}
}

// firstMailboxID pulls one id out of a Mailbox/get reply.
func firstMailboxID(b *testing.B, body string) string {
	b.Helper()
	const key = `"id":"`
	i := strings.Index(body, key)
	if i < 0 {
		b.Fatalf("no mailbox id in %s", body)
	}
	rest := body[i+len(key):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		b.Fatalf("malformed id in %s", body)
	}
	return rest[:j]
}

// The other half of the question: with the folder count fixed, does cost track
// the number of messages the client actually asked for? If it does not, the
// work is elsewhere.
func BenchmarkAnswerSizeEmailQueryGet(b *testing.B) {
	for _, limit := range []int{1, 10, 50} {
		b.Run(fmt.Sprintf("limit=%d", limit), func(b *testing.B) {
			s := benchServer(b, 20, 10)
			body := fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
				"methodCalls":[
					["Email/query",{"accountId":"u1@example.com","limit":%d,
						"sort":[{"property":"receivedAt","isAscending":false}]},"q0"],
					["Email/get",{"accountId":"u1@example.com",
						"#ids":{"resultOf":"q0","name":"Email/query","path":"/ids"},
						"properties":["id","subject","receivedAt"]},"g0"]]}`, limit)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				post(b, s, body)
			}
		})
	}
}
