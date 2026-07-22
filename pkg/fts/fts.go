// Package fts defines the full-text-search engine contract: the build-key
// model an indexer feeds documents through, the query/result shapes a lookup
// speaks, and the per-user index handle engines implement. The interface is
// modelled on the reference count-backend contract so engine behaviour can
// be checked against it (traceability in docs/FTS.md).
package fts

import "context"

// BuildKeyType selects which part of a message a build key describes.
type BuildKeyType int

const (
	// KeyHeader is a top-level message header field.
	KeyHeader BuildKeyType = iota
	// KeyMIMEHeader is a header field of a nested MIME part.
	KeyMIMEHeader
	// KeyBodyPart is a decoded text body part.
	KeyBodyPart
	// KeyBodyPartBinary is an undecoded binary part; only offered to engines
	// that declare BinaryMIMEParts.
	KeyBodyPartBinary
)

// BuildKey scopes the BuildMore stream that follows it: which message (UID),
// which part type, and — for headers — the lowercased field name.
type BuildKey struct {
	UID         uint32
	Type        BuildKeyType
	HdrName     string
	ContentType string
}

// UserRef identifies the user whose index is being opened.
type UserRef struct {
	Username string
	// IndexRoot is the resolved index root (INDEX= override → mail path →
	// home), under which the engine places its on-disk layout.
	IndexRoot string
	// Driver is the user's mailbox storage driver (maildir / mdbox / sdbox).
	// A path-derived engine uses it so its on-disk layout matches the shared
	// per-folder index convention instead of a flat folder-name path (#654).
	Driver string
	// Separator is the IMAP hierarchy separator, for mapping a folder name to
	// its on-disk sub-path. Empty is treated as "/".
	Separator string
}

// MailboxRef identifies one mailbox within a user's index.
type MailboxRef struct {
	// GUID is the stable folder identity (OBJECTID machinery); survives
	// renames.
	GUID string
	// Name is the folder's full name, for engines whose on-disk layout is
	// path-derived (flatcurve).
	Name        string
	UIDValidity uint32
}

// Caps declares what an engine supports so the core can adapt query building
// and the indexing stream.
type Caps struct {
	// Tokenized engines receive one token per BuildMore call and
	// pre-expanded query tokens (flatcurve model).
	Tokenized bool
	// Positions engines store positional data and can run Phrase queries.
	Positions bool
	// Substring engines match inside tokens, not only by prefix.
	Substring bool
	// Scoring engines fill Result.Scores.
	Scoring bool
	// BinaryMIMEParts engines accept KeyBodyPartBinary streams.
	BinaryMIMEParts bool
}

// Engine is a registered FTS driver (flatcurve, bleve, ...).
type Engine interface {
	Name() string
	Caps() Caps
	OpenUser(ctx context.Context, user UserRef) (UserIndex, error)
	Close() error
}

// OptimizeNotifier is an optional Engine capability (#715): an engine whose
// on-disk shard/segment count grows unboundedly without periodic compaction
// (flatcurve) implements it so the service can be told when a mailbox
// crosses its optimize threshold and should be queued for background
// optimization. fn is called synchronously from deep inside the engine's
// write path (flatcurve: under its per-user lock, right after a shard
// rotation) — it MUST NOT block or perform any compaction itself, only
// enqueue for a background worker to pick up later.
type OptimizeNotifier interface {
	SetOptimizeCallback(fn func(user UserRef, mbox MailboxRef))
}

// UserIndex is the per-user handle. All writes are serialised by the caller
// (the yarilo-fts service owns the index; pkg/locks guards the directory).
type UserIndex interface {
	// Checkpoint returns the per-mailbox indexing checkpoint: the highest indexed
	// UID, the UIDVALIDITY it was recorded under, and the settings checksum. A zero
	// checkpoint means the mailbox was never indexed; a stored UIDVALIDITY that no
	// longer matches the mailbox means it was recreated and the checkpoint is stale
	// (the caller must reset — #638). uidValidity reads back as 0 for legacy
	// checkpoints written before it was tracked.
	Checkpoint(mbox MailboxRef) (lastUID, uidValidity, settingsChecksum uint32, err error)
	SetCheckpoint(mbox MailboxRef, lastUID, uidValidity, settingsChecksum uint32) error

	BeginUpdate(mbox MailboxRef) (Update, error)
	Expunge(mbox MailboxRef, uid uint32) error
	// Rescan reconciles the index against the authoritative UID set: deletes
	// documents whose UID is absent and reports which present UIDs are
	// missing from the index so the caller can reindex exactly those.
	Rescan(mbox MailboxRef, present []uint32) (missing []uint32, err error)
	// Optimize compacts every mailbox owned by the user — the whole-user
	// semantics yarilo-admin fts optimize keeps (#715).
	Optimize() error
	// OptimizeMailbox compacts sealed shards for exactly one mailbox — used
	// by the background auto-optimize queue (#715) so one large mailbox's
	// compaction doesn't block indexing of a user's other mailboxes, unlike
	// the whole-user Optimize.
	OptimizeMailbox(mbox MailboxRef) error
	// Refresh makes writes committed by earlier updates visible to Lookup.
	Refresh() error

	Lookup(mbox MailboxRef, q Query) (Result, error)
	Close() error
}

// Update is one indexing session for one mailbox. Keys and token/text data
// arrive interleaved: SetBuildKey scopes the following BuildMore calls.
type Update interface {
	// SetBuildKey starts a new part. Returning accept=false skips the part:
	// the caller must not stream its data.
	SetBuildKey(k BuildKey) (accept bool, err error)
	// BuildMore appends data for the current key. For Tokenized engines each
	// call carries exactly one token; otherwise a chunk of valid UTF-8.
	BuildMore(data []byte) error
	Commit() error
	Rollback() error
}

// FieldKind selects the search field of a query term.
type FieldKind int

const (
	// FieldBody searches decoded body text only (IMAP SEARCH BODY).
	FieldBody FieldKind = iota
	// FieldText searches headers and body (IMAP SEARCH TEXT).
	FieldText
	// FieldHeader searches one header field (IMAP SEARCH HEADER <name>).
	FieldHeader
)

// Word is one search word expanded to its OR-variants (raw, unfiltered,
// filtered), per the reference query-expansion model: a document matches
// the word when it matches any variant.
type Word struct {
	Variants []string
}

// Term is one search criterion: all Words must match (AND), within Field.
// Phrase carries the original multi-word text for engines with Positions;
// engines without positions ignore it (upstream flatcurve behaviour).
type Term struct {
	Field   FieldKind
	HdrName string
	Words   []Word
	Phrase  string
	Not     bool
}

// Query is the set of terms the engine evaluates. AndTerms selects AND vs OR
// composition (FTS_LOOKUP_FLAG_AND_ARGS).
type Query struct {
	Terms    []Term
	AndTerms bool
}

// Score is one UID's relevancy score.
type Score struct {
	UID   uint32
	Value float64
}

// Result is a lookup outcome for one mailbox. Definite UIDs matched
// exactly; Maybe UIDs need re-verification against the raw message (engines
// that can only over-approximate, e.g. pooled-header matches in flatcurve).
// All slices are sorted by UID ascending.
type Result struct {
	Definite []uint32
	Maybe    []uint32
	Scores   []Score
}

// MergeScoresAnd folds src into dest for an AND composition: UIDs present
// in both keep the higher score; dest-only UIDs are deliberately left as-is.
// Both slices must be sorted by UID; dest is modified in place.
func MergeScoresAnd(dest []Score, src []Score) []Score {
	di, si := 0, 0
	for di < len(dest) && si < len(src) {
		switch {
		case dest[di].UID < src[si].UID:
			di++
		case dest[di].UID > src[si].UID:
			si++
		default:
			if dest[di].Value < src[si].Value {
				dest[di].Value = src[si].Value
			}
			di++
			si++
		}
	}
	return dest
}

// MergeScoresOr merges src and dest for an OR composition: the union of
// both, common UIDs keeping the
// higher score. Both inputs must be sorted by UID; returns a new slice.
func MergeScoresOr(dest []Score, src []Score) []Score {
	out := make([]Score, 0, len(dest)+len(src))
	di, si := 0, 0
	for di < len(dest) || si < len(src) {
		switch {
		case di == len(dest):
			out = append(out, src[si])
			si++
		case si == len(src):
			out = append(out, dest[di])
			di++
		case src[si].UID < dest[di].UID:
			out = append(out, src[si])
			si++
		case src[si].UID > dest[di].UID:
			out = append(out, dest[di])
			di++
		default:
			if src[si].Value > dest[di].Value {
				out = append(out, src[si])
			} else {
				out = append(out, dest[di])
			}
			di++
			si++
		}
	}
	return out
}
