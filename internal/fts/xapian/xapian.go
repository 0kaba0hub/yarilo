//go:build flatcurve

// Package xapian is a thin cgo binding over the Xapian C++ search library.
// It knows nothing about mail, IMAP, or any yarilo domain type — it exposes
// only writable/read databases, documents, queries and search results, so it
// can later be lifted into a standalone module (go-xapian) and reused. The
// mail-specific FTS engine lives in internal/fts/flatcurve and is built on top
// of this package.
//
// cgo cannot call C++ directly (name mangling, templates, exceptions, RAII),
// so every call crosses through the extern "C" shim in shim.cc / shim.h, which
// converts Xapian C++ exceptions into malloc'd error strings. All handles are
// opaque pointers owned by the caller until the matching Close/Free.
//
// The binding compiles only under the "flatcurve" build tag and requires
// libxapian with its development headers at build time.
package xapian

/*
#cgo CXXFLAGS: -std=c++17
#cgo darwin CXXFLAGS: -I/opt/homebrew/include
#cgo darwin LDFLAGS: -L/opt/homebrew/lib
#cgo LDFLAGS: -lxapian

#include <stdlib.h>
#include "shim.h"
*/
import "C"

import (
	"errors"
	"unsafe"
)

func takeErr(cerr *C.char) error {
	if cerr == nil {
		return errors.New("xapian: unknown error")
	}
	defer C.free(unsafe.Pointer(cerr))
	return errors.New("xapian: " + C.GoString(cerr))
}

// WDB is a writable Xapian database handle.
type WDB struct{ h unsafe.Pointer }

// OpenWDB opens (creating if absent) a writable database at path.
func OpenWDB(path string) (*WDB, error) {
	cp := C.CString(path)
	defer C.free(unsafe.Pointer(cp))
	var cerr *C.char
	h := C.fcx_wdb_open(cp, &cerr)
	if h == nil {
		return nil, takeErr(cerr)
	}
	return &WDB{h: h}, nil
}

// Commit flushes pending changes to disk.
func (w *WDB) Commit() error {
	var cerr *C.char
	if C.fcx_wdb_commit(w.h, &cerr) != 0 {
		return takeErr(cerr)
	}
	return nil
}

// Close releases the handle. Idempotent.
func (w *WDB) Close() {
	if w.h != nil {
		C.fcx_wdb_close(w.h)
		w.h = nil
	}
}

// ReplaceDocument stores d under docid, replacing any existing document.
func (w *WDB) ReplaceDocument(docid uint32, d *Doc) error {
	var cerr *C.char
	if C.fcx_wdb_replace_document(w.h, C.uint(docid), d.h, &cerr) != 0 {
		return takeErr(cerr)
	}
	return nil
}

// DeleteDocument removes docid. existed reports whether it was present;
// a not-found document is not an error.
func (w *WDB) DeleteDocument(docid uint32) (existed bool, err error) {
	var cerr *C.char
	var cex C.int
	if C.fcx_wdb_delete_document(w.h, C.uint(docid), &cex, &cerr) != 0 {
		return false, takeErr(cerr)
	}
	return cex != 0, nil
}

// SetMetadata stores an arbitrary key/value pair in the database metadata.
func (w *WDB) SetMetadata(key, value string) error {
	ck := C.CString(key)
	cv := C.CString(value)
	defer C.free(unsafe.Pointer(ck))
	defer C.free(unsafe.Pointer(cv))
	var cerr *C.char
	if C.fcx_wdb_set_metadata(w.h, ck, cv, &cerr) != 0 {
		return takeErr(cerr)
	}
	return nil
}

// GetMetadata reads a metadata value; a missing key returns "".
func (w *WDB) GetMetadata(key string) (string, error) {
	ck := C.CString(key)
	defer C.free(unsafe.Pointer(ck))
	var cerr *C.char
	cv := C.fcx_wdb_get_metadata(w.h, ck, &cerr)
	if cv == nil {
		return "", takeErr(cerr)
	}
	defer C.free(unsafe.Pointer(cv))
	return C.GoString(cv), nil
}

// DocCount returns the number of documents in the writable database.
func (w *WDB) DocCount() (uint32, error) {
	var cerr *C.char
	n := C.fcx_wdb_get_doccount(w.h, &cerr)
	if cerr != nil {
		return 0, takeErr(cerr)
	}
	return uint32(n), nil
}

// DocExists reports whether docid is present.
func (w *WDB) DocExists(docid uint32) (bool, error) {
	var cerr *C.char
	r := C.fcx_wdb_doc_exists(w.h, C.uint(docid), &cerr)
	if r < 0 {
		return false, takeErr(cerr)
	}
	return r == 1, nil
}

// DB is a read-only Xapian database, optionally combining several on-disk
// shards into one searchable view.
type DB struct{ h unsafe.Pointer }

// OpenDBMulti opens paths as one combined read-only database. An empty paths
// slice returns (nil, nil).
func OpenDBMulti(paths []string) (*DB, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	cpaths := make([]*C.char, len(paths))
	for i, p := range paths {
		cpaths[i] = C.CString(p)
	}
	defer func() {
		for _, cp := range cpaths {
			C.free(unsafe.Pointer(cp))
		}
	}()
	var cerr *C.char
	h := C.fcx_db_open_multi(&cpaths[0], C.size_t(len(paths)), &cerr)
	if h == nil {
		return nil, takeErr(cerr)
	}
	return &DB{h: h}, nil
}

// Close releases the handle. Idempotent.
func (d *DB) Close() {
	if d.h != nil {
		C.fcx_db_close(d.h)
		d.h = nil
	}
}

// LastDocID returns the highest document id across the combined shards.
func (d *DB) LastDocID() (uint32, error) {
	var cerr *C.char
	n := C.fcx_db_get_lastdocid(d.h, &cerr)
	if cerr != nil {
		return 0, takeErr(cerr)
	}
	return uint32(n), nil
}

// DocIDs returns every document id in ascending order.
func (d *DB) DocIDs() ([]uint32, error) {
	var out []uint32
	buf := make([]C.uint, 4096)
	prev := C.uint(0)
	for {
		var cerr *C.char
		n := C.fcx_db_docids(d.h, prev, &buf[0], C.size_t(len(buf)), &cerr)
		if n < 0 {
			return nil, takeErr(cerr)
		}
		for i := 0; i < int(n); i++ {
			out = append(out, uint32(buf[i]))
		}
		if int(n) < len(buf) {
			return out, nil
		}
		prev = buf[n-1]
	}
}

// Compact writes a single optimized copy of the combined database to dest.
func (d *DB) Compact(dest string) error {
	cd := C.CString(dest)
	defer C.free(unsafe.Pointer(cd))
	var cerr *C.char
	if C.fcx_db_compact(d.h, cd, &cerr) != 0 {
		return takeErr(cerr)
	}
	return nil
}

// Doc is a Xapian document under construction.
type Doc struct{ h unsafe.Pointer }

// NewDoc allocates an empty document. The caller must Free it (or hand it to
// WDB.ReplaceDocument, which does not take ownership — Free it afterwards).
func NewDoc() *Doc { return &Doc{h: C.fcx_doc_new()} }

// Free releases the document. Idempotent.
func (d *Doc) Free() {
	if d.h != nil {
		C.fcx_doc_free(d.h)
		d.h = nil
	}
}

// AddTerm indexes a free-text term.
func (d *Doc) AddTerm(term string) error {
	ct := C.CString(term)
	defer C.free(unsafe.Pointer(ct))
	var cerr *C.char
	if C.fcx_doc_add_term(d.h, ct, &cerr) != 0 {
		return takeErr(cerr)
	}
	return nil
}

// AddBooleanTerm indexes a boolean (filter) term.
func (d *Doc) AddBooleanTerm(term string) error {
	ct := C.CString(term)
	defer C.free(unsafe.Pointer(ct))
	var cerr *C.char
	if C.fcx_doc_add_boolean_term(d.h, ct, &cerr) != 0 {
		return takeErr(cerr)
	}
	return nil
}

// Query is a Xapian query tree node.
type Query struct{ h unsafe.Pointer }

// QueryWildcard builds a wildcard (prefix) query from pattern.
func QueryWildcard(pattern string) (*Query, error) {
	cp := C.CString(pattern)
	defer C.free(unsafe.Pointer(cp))
	var cerr *C.char
	h := C.fcx_query_wildcard(cp, &cerr)
	if h == nil {
		return nil, takeErr(cerr)
	}
	return &Query{h: h}, nil
}

// QueryTerm builds an exact-term query.
func QueryTerm(term string) (*Query, error) {
	ct := C.CString(term)
	defer C.free(unsafe.Pointer(ct))
	var cerr *C.char
	h := C.fcx_query_term(ct, &cerr)
	if h == nil {
		return nil, takeErr(cerr)
	}
	return &Query{h: h}, nil
}

// QueryMatchAll builds a query that matches every document.
func QueryMatchAll() (*Query, error) {
	var cerr *C.char
	h := C.fcx_query_match_all(&cerr)
	if h == nil {
		return nil, takeErr(cerr)
	}
	return &Query{h: h}, nil
}

// Op selects how QueryCombine joins two subqueries.
type Op int

const (
	OpAND    = Op(C.FCX_OP_AND)
	OpOR     = Op(C.FCX_OP_OR)
	OpANDNOT = Op(C.FCX_OP_AND_NOT)
)

// QueryCombine joins a and b under op. It consumes a and b (both are freed
// here) and returns the combined query.
func QueryCombine(op Op, a, b *Query) (*Query, error) {
	defer a.Free()
	defer b.Free()
	var cerr *C.char
	h := C.fcx_query_combine(C.int(op), a.h, b.h, &cerr)
	if h == nil {
		return nil, takeErr(cerr)
	}
	return &Query{h: h}, nil
}

// Free releases the query. Safe on a nil receiver or nil handle.
func (q *Query) Free() {
	if q != nil && q.h != nil {
		C.fcx_query_free(q.h)
		q.h = nil
	}
}

// MSetEntry is one search hit: a document id and its relevance weight.
type MSetEntry struct {
	DocID  uint32
	Weight float64
}

// Search runs q against the read database and returns the ranked hits.
func (d *DB) Search(q *Query) ([]MSetEntry, error) {
	var cerr *C.char
	m := C.fcx_db_search(d.h, q.h, &cerr)
	if m == nil {
		return nil, takeErr(cerr)
	}
	defer C.fcx_mset_free(m)
	n := int(C.fcx_mset_size(m))
	out := make([]MSetEntry, 0, n)
	for i := 0; i < n; i++ {
		var w C.double
		docid := C.fcx_mset_docid(m, C.size_t(i), &w)
		out = append(out, MSetEntry{DocID: uint32(docid), Weight: float64(w)})
	}
	return out, nil
}
