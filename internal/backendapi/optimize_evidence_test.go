package backendapi

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

type optimizeAllResponse struct {
	Folders []struct {
		Folder string     `json:"folder"`
		Before *foldSizes `json:"before"`
		After  *foldSizes `json:"after"`
	} `json:"folders"`
	MapFolded   *bool      `json:"map_folded"`
	MapBefore   *foldSizes `json:"map_before"`
	MapAfter    *foldSizes `json:"map_after"`
	FoldedCount int        `json:"folded_count"`
	FailedCount int        `json:"failed_count"`
}

func optimizeAllOn(t *testing.T, ts *httptest.Server, user string) optimizeAllResponse {
	t.Helper()
	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/index/optimize", "",
		map[string]any{"user": user, "all": true})
	if status != 200 {
		t.Fatalf("optimize status=%d body=%s", status, body)
	}
	var out optimizeAllResponse
	decodeJSONBody(t, body, &out)
	return out
}

// A successful exit and an unfolded account look identical from outside, which
// is how #1272 stayed invisible to the operator who ran it. The response has to
// carry the bytes: a fold that moved the log reports a smaller log after than
// before, and the counts say how many folders it covered.
func TestOptimizeAllReportsTheBytesItMoved(t *testing.T) {
	ts, root := storageTestServer(t)
	const user = "alice@example.com"

	uc, err := newAdminUserContext(t, ts, root, user)
	if err != nil {
		t.Fatal(err)
	}
	defer uc.cleanup()
	for i := 0; i < 50; i++ {
		uc.deliver(t, "Subject: m\r\n\r\nbody\r\n")
	}

	resp := optimizeAllOn(t, ts, user)
	if resp.FoldedCount != len(resp.Folders) || resp.FoldedCount == 0 {
		t.Fatalf("folded_count = %d against %d folder entries", resp.FoldedCount, len(resp.Folders))
	}
	if resp.FailedCount != 0 {
		t.Errorf("failed_count = %d, want 0", resp.FailedCount)
	}
	inbox := resp.Folders[0]
	if inbox.Before == nil || inbox.After == nil {
		t.Fatal("no journal sizes in the response; the fold is unverifiable from it")
	}
	if inbox.Before.LogBytes <= inbox.After.LogBytes {
		t.Errorf("log %d -> %d: the response does not show a fold that happened",
			inbox.Before.LogBytes, inbox.After.LogBytes)
	}
	if inbox.After.BaseBytes <= inbox.Before.BaseBytes {
		t.Errorf("base %d -> %d: folding the log must grow the base",
			inbox.Before.BaseBytes, inbox.After.BaseBytes)
	}
}

// The row the reporting exists for: a second fold has nothing to fold, and must
// say so in numbers rather than by returning the same cheerful 200. This is the
// shape #1272 was reported as — a successful call over an account nothing
// happened to.
func TestSecondOptimizeReportsThatNothingMoved(t *testing.T) {
	ts, root := storageTestServer(t)
	const user = "alice@example.com"

	uc, err := newAdminUserContext(t, ts, root, user)
	if err != nil {
		t.Fatal(err)
	}
	defer uc.cleanup()
	for i := 0; i < 50; i++ {
		uc.deliver(t, "Subject: m\r\n\r\nbody\r\n")
	}

	optimizeAllOn(t, ts, user)
	second := optimizeAllOn(t, ts, user)

	inbox := second.Folders[0]
	if inbox.Before == nil || inbox.After == nil {
		t.Fatal("no journal sizes on the second call")
	}
	if inbox.Before.LogBytes != inbox.After.LogBytes || inbox.Before.BaseBytes != inbox.After.BaseBytes {
		t.Errorf("second fold moved bytes: base %d -> %d, log %d -> %d",
			inbox.Before.BaseBytes, inbox.After.BaseBytes, inbox.Before.LogBytes, inbox.After.LogBytes)
	}
}

// The map is the other half of an mdbox fold, and it is the half that was
// silently skipped once already (#1267). Its sizes are reported on the driver
// that has a map — and, on a driver that has none, are absent rather than zero,
// because zeros would read as "measured, and empty".
func TestMapSizesReportedOnMdboxAndAbsentElsewhere(t *testing.T) {
	t.Run("mdbox reports the map it folded", func(t *testing.T) {
		ts, root := storageTestServerMdbox(t)
		const user = "alice@example.com"
		seedMdbox(t, root, user, 5)

		resp := optimizeAllOn(t, ts, user)
		if resp.MapBefore == nil || resp.MapAfter == nil {
			t.Fatal("no map sizes on an mdbox account")
		}
		if resp.MapBefore.LogBytes <= 0 {
			t.Fatalf("the seed left no map log to fold (%d); the row proves nothing", resp.MapBefore.LogBytes)
		}
		// Folding the map removes its log, which is not the same fact as an
		// empty one; -1 is how the absence is reported.
		if resp.MapAfter.LogBytes != -1 {
			t.Errorf("map log after the fold = %d, want -1 (removed)", resp.MapAfter.LogBytes)
		}
		if resp.MapAfter.BaseBytes <= resp.MapBefore.BaseBytes {
			t.Errorf("map base %d -> %d: the fold must have written it",
				resp.MapBefore.BaseBytes, resp.MapAfter.BaseBytes)
		}
	})

	t.Run("maildir reports no map at all", func(t *testing.T) {
		ts, root := storageTestServer(t)
		const user = "alice@example.com"
		uc, err := newAdminUserContext(t, ts, root, user)
		if err != nil {
			t.Fatal(err)
		}
		defer uc.cleanup()
		uc.deliver(t, "Subject: m\r\n\r\nbody\r\n")

		resp := optimizeAllOn(t, ts, user)
		if resp.MapBefore != nil || resp.MapAfter != nil {
			t.Errorf("map sizes reported for a driver with no map: %+v / %+v", resp.MapBefore, resp.MapAfter)
		}
	})
}

func seedMdbox(t *testing.T, root, user string, n int) {
	t.Helper()
	info := &mailbox.UserInfo{Username: user, Home: maildirHome(root, user)}
	box := mdbox.New().OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatalf("init mdbox: %v", err)
	}
	defer box.Close() //nolint:errcheck
	idx := file.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	folder, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	for i := range n {
		body := fmt.Sprintf("Subject: m%d\r\n\r\nbody\r\n", i)
		uid, uerr := idx.AllocateUID(folder.ID)
		if uerr != nil {
			t.Fatalf("allocate uid: %v", uerr)
		}
		filename, _, _, serr := box.Save("INBOX", io.NopCloser(bytes.NewBufferString(body)), uid, int64(len(body)), nil, [16]byte{})
		if serr != nil {
			t.Fatalf("save: %v", serr)
		}
		if aerr := idx.AppendMessage(folder.ID, &mailbox.MessageMeta{UID: uid, Filename: filename, Size: uint32(len(body))}); aerr != nil {
			t.Fatalf("append: %v", aerr)
		}
	}
}
