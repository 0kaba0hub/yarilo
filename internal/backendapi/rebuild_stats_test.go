package backendapi

import (
	"reflect"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The API answer carries every field the rebuild reports.
//
// FilesNormalised was added to the driver's stats and not to the response, so
// an operator could not see whether a rebuild had rewritten anything (#1687).
func TestTheAPIAnswerCarriesEveryRebuildStat(t *testing.T) {
	// ExpungedUIDs drives the FTS invalidation and is deliberately not in the
	// answer: it is per-folder detail the caller does not act on.
	notReported := map[string]bool{"ExpungedUIDs": true}

	answer := map[string]bool{}
	rt := reflect.TypeOf(storageRebuildStats{})
	for i := 0; i < rt.NumField(); i++ {
		answer[rt.Field(i).Name] = true
	}
	st := reflect.TypeOf(mailbox.StorageRebuildStats{})
	for i := 0; i < st.NumField(); i++ {
		name := st.Field(i).Name
		if notReported[name] {
			continue
		}
		if !answer[name] {
			t.Errorf("the rebuild reports %s and the API answer does not carry it: an "+
				"operator cannot see what the rebuild did", name)
		}
	}
	if st.NumField() < 7 {
		t.Fatalf("StorageRebuildStats has %d fields; the walk is not reaching them", st.NumField())
	}
}
