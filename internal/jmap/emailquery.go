package jmap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// filterEvaluator returns the evaluator this deployment can answer filters
// with: the full-text one when yarilo-fts is wired, the index-only one
// otherwise -- which refuses text conditions by name rather than answering
// them from what it can see.
func (s *Server) filterEvaluator(h *userHandle) filterEvaluator {
	if s.opts.FTS != nil && s.opts.FTS.Client != nil && s.opts.FTS.Chain != nil {
		slog.Debug("jmap: filter evaluator selected", "evaluator", "full-text")
		return s.newFTSEvaluator(h)
	}
	// Which of the three is missing decides how a text condition is answered,
	// and from outside the answer looks the same either way (#1279).
	slog.Debug("jmap: filter evaluator selected", "evaluator", "index-only",
		"fts_options", s.opts.FTS != nil,
		"fts_client", s.opts.FTS != nil && s.opts.FTS.Client != nil,
		"language_chain", s.opts.FTS != nil && s.opts.FTS.Chain != nil)
	return indexEvaluator{}
}

// emailQuery implements Email/query (RFC 8621 §4.4).
func (s *Server) emailQuery(ctx context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req jmapcore.EmailQueryRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	if req.Filter != nil && req.Filter.Operator != "" {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrUnsupportedFilter,
			Description: "filter operators are not supported yet"}
	}
	// The evaluator, not the query body, decides what can be answered: adding
	// full-text support means supplying an evaluator that names fewer
	// conditions, with nothing here to change.
	eval := s.filterEvaluator(h)
	if unsupported := eval.unsupported(req.Filter); len(unsupported) > 0 {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrUnsupportedFilter,
			Description: "conditions not supported yet: " + strings.Join(unsupported, ", ")}
	}
	if merr := checkEmailSort(req.Sort); merr != nil {
		return nil, merr
	}

	scope, err := s.queryScope(h, req.Filter)
	if err != nil {
		return nil, storeFailure("Email/query scope failed", accountID, err)
	}

	if merr := s.checkQueryFolders(scope, req.Filter); merr != nil {
		return nil, merr
	}
	// Every folder is resolved before any message is read: the lookups run
	// against one another rather than one per pass through the loop below.
	if merr := s.prepareScope(ctx, h, accountID, eval, scope, req.Filter); merr != nil {
		return nil, merr
	}

	matched := make([]queryHit, 0, 64)
	for _, f := range scope.folders {
		metas, err := mailbox.ReadMessages(h.idx, f.id, mailbox.SeqSet{{From: 1, To: 0}})
		if err != nil {
			return nil, storeFailure("Email/query read of "+f.name, accountID, err)
		}
		for _, m := range metas {
			if !eval.match(m, f, scope, req.Filter) {
				continue
			}
			matched = append(matched, queryHit{id: emailID(m), received: m.InternalDate, size: m.Size})
		}
	}
	sortEmailHits(matched, req.Sort)

	ids := make([]string, 0, len(matched))
	for _, hit := range matched {
		ids = append(ids, hit.id)
	}
	total := uint(len(ids))

	pos := req.Position
	if pos < 0 {
		pos = len(ids) + pos
		if pos < 0 {
			pos = 0
		}
	}
	if pos > len(ids) {
		pos = len(ids)
	}
	ids = ids[pos:]

	// The ceiling bounds the server's work; the response reports the limit that
	// was applied, so a client can see it was cut short and page with position
	// (RFC 8620 §5.5).
	applied := jmapcore.EffectiveLimit(req.Limit, s.opts.QueryMaxLimit)
	if applied > 0 && uint(len(ids)) > applied {
		ids = ids[:applied]
	}

	resp := jmapcore.QueryResponse{
		AccountID:           accountID,
		QueryState:          scope.state(req.Filter, req.Sort),
		CanCalculateChanges: false,
		Position:            pos,
		IDs:                 ids,
	}
	if applied > 0 {
		limit := applied
		resp.Limit = &limit
	}
	if req.CalculateTotal {
		resp.Total = &total
	}
	return resp, nil
}

// queryHit is one match, carrying just what the sort needs.
type queryHit struct {
	id       string
	received time.Time
	size     uint32
}

// scopeFolder is a folder the query reads.
type scopeFolder struct {
	name string
	id   uint64
	// guid is the folder identity the full-text index is keyed by; empty means
	// the folder has none and cannot be searched (#1183).
	guid        string
	mailboxID   string
	uidValidity uint32
	highestMod  uint64
}

// queryScope is the set of folders a query reads plus what identifies their
// state.
type queryScope struct {
	folders []scopeFolder
	// byMailboxID maps the JMAP mailbox id to the folder id, for the
	// inMailbox conditions.
	byMailboxID map[string]uint64
}

// queryScope resolves which folders the filter can match in. A filter naming a
// mailbox reads only that one, which is what keeps a delivery elsewhere from
// moving this query's state.
func (s *Server) queryScope(h *userHandle, f *jmapcore.EmailFilter) (*queryScope, error) {
	entries, err := h.box.ListFolders()
	if err != nil {
		return nil, fmt.Errorf("jmap: list folders: %w", err)
	}
	scope := &queryScope{byMailboxID: map[string]uint64{}}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Selectable {
			names = append(names, e.Name)
		}
	}
	// Sorted so the scope, and therefore the state string, does not depend on
	// the order the storage driver happened to list folders in.
	sort.Strings(names)

	for _, name := range names {
		folder, err := h.idx.OpenFolder(name, 0)
		if err != nil {
			return nil, fmt.Errorf("jmap: open folder %q: %w", name, err)
		}
		sf := scopeFolder{
			name:        name,
			id:          folder.ID,
			guid:        ftsMailboxGUID(folder.GUID),
			mailboxID:   mailboxID(folder.GUID),
			uidValidity: folder.UIDValidity,
			highestMod:  folder.HighestModSeq,
		}
		scope.byMailboxID[sf.mailboxID] = sf.id
		scope.folders = append(scope.folders, sf)
	}
	if f != nil && f.InMailbox != nil {
		want := *f.InMailbox
		kept := scope.folders[:0]
		for _, sf := range scope.folders {
			if sf.mailboxID == want {
				kept = append(kept, sf)
			}
		}
		scope.folders = kept
	}
	return scope, nil
}

// state identifies the result set. It covers the filter and the sort, since two
// queries differing only in either produce different lists; the composition of
// the folder set, since creating or deleting a folder changes the result
// without moving any surviving folder's modseq; and each folder's
// (UIDVALIDITY, HIGHESTMODSEQ), which move on delivery, flag change and expunge.
//
// It is not a change log: a flag change in a folder the query reads moves the
// state even when the filter does not depend on flags. That is coarser than
// necessary and finer than a digest of everything — the precise answer needs a
// change journal, which is a later phase.
func (q *queryScope) state(f *jmapcore.EmailFilter, sort []jmapcore.Comparator) string {
	h := sha256.New()
	filterJSON, _ := json.Marshal(f)
	sortJSON, _ := json.Marshal(sort)
	fmt.Fprintf(h, "f:%s\x00s:%s\x00", filterJSON, sortJSON)
	for _, sf := range q.folders {
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00", sf.mailboxID, sf.uidValidity, sf.highestMod)
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// checkEmailSort refuses a comparator the server cannot honour rather than
// returning a different order than the one asked for.
func checkEmailSort(cmps []jmapcore.Comparator) *jmapcore.MethodError {
	for _, c := range cmps {
		switch c.Property {
		case "receivedAt", "size":
		default:
			return &jmapcore.MethodError{Type: jmapcore.ErrUnsupportedSort,
				Description: fmt.Sprintf("cannot sort on %q", c.Property)}
		}
	}
	return nil
}

// sortEmailHits orders the result. The id breaks every tie, so the order is
// total: two runs of the same query must agree, or a client paging with
// position sees messages twice or not at all.
func sortEmailHits(hits []queryHit, cmps []jmapcore.Comparator) {
	if len(cmps) == 0 {
		// The default is newest first (RFC 8621 §4.4.2), which is what a mail
		// client renders without asking.
		cmps = []jmapcore.Comparator{{Property: "receivedAt", IsAscending: boolPtr(false)}}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		for _, c := range cmps {
			less, equal := compareHits(hits[i], hits[j], c.Property)
			if equal {
				continue
			}
			if c.Ascending() {
				return less
			}
			return !less
		}
		return hits[i].id < hits[j].id
	})
}

func compareHits(a, b queryHit, property string) (less, equal bool) {
	switch property {
	case "size":
		return a.size < b.size, a.size == b.size
	default:
		return a.received.Before(b.received), a.received.Equal(b.received)
	}
}

func boolPtr(b bool) *bool { return &b }
