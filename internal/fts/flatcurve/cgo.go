//go:build flatcurve

package flatcurve

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
		return errors.New("fts/flatcurve: unknown xapian error")
	}
	defer C.free(unsafe.Pointer(cerr))
	return errors.New("fts/flatcurve: " + C.GoString(cerr))
}

type xWDB struct{ h unsafe.Pointer }

func openWDB(path string) (*xWDB, error) {
	cp := C.CString(path)
	defer C.free(unsafe.Pointer(cp))
	var cerr *C.char
	h := C.fcx_wdb_open(cp, &cerr)
	if h == nil {
		return nil, takeErr(cerr)
	}
	return &xWDB{h: h}, nil
}

func (w *xWDB) commit() error {
	var cerr *C.char
	if C.fcx_wdb_commit(w.h, &cerr) != 0 {
		return takeErr(cerr)
	}
	return nil
}

func (w *xWDB) close() {
	if w.h != nil {
		C.fcx_wdb_close(w.h)
		w.h = nil
	}
}

func (w *xWDB) replaceDocument(docid uint32, d *xDoc) error {
	var cerr *C.char
	if C.fcx_wdb_replace_document(w.h, C.uint(docid), d.h, &cerr) != 0 {
		return takeErr(cerr)
	}
	return nil
}

func (w *xWDB) deleteDocument(docid uint32) (existed bool, err error) {
	var cerr *C.char
	var cex C.int
	if C.fcx_wdb_delete_document(w.h, C.uint(docid), &cex, &cerr) != 0 {
		return false, takeErr(cerr)
	}
	return cex != 0, nil
}

func (w *xWDB) setMetadata(key, value string) error {
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

func (w *xWDB) getMetadata(key string) (string, error) {
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

func (w *xWDB) docCount() (uint32, error) {
	var cerr *C.char
	n := C.fcx_wdb_get_doccount(w.h, &cerr)
	if cerr != nil {
		return 0, takeErr(cerr)
	}
	return uint32(n), nil
}

func (w *xWDB) docExists(docid uint32) (bool, error) {
	var cerr *C.char
	r := C.fcx_wdb_doc_exists(w.h, C.uint(docid), &cerr)
	if r < 0 {
		return false, takeErr(cerr)
	}
	return r == 1, nil
}

type xDB struct{ h unsafe.Pointer }

func openDBMulti(paths []string) (*xDB, error) {
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
	return &xDB{h: h}, nil
}

func (d *xDB) close() {
	if d.h != nil {
		C.fcx_db_close(d.h)
		d.h = nil
	}
}

func (d *xDB) lastDocID() (uint32, error) {
	var cerr *C.char
	n := C.fcx_db_get_lastdocid(d.h, &cerr)
	if cerr != nil {
		return 0, takeErr(cerr)
	}
	return uint32(n), nil
}

func (d *xDB) docIDs() ([]uint32, error) {
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

func (d *xDB) compact(dest string) error {
	cd := C.CString(dest)
	defer C.free(unsafe.Pointer(cd))
	var cerr *C.char
	if C.fcx_db_compact(d.h, cd, &cerr) != 0 {
		return takeErr(cerr)
	}
	return nil
}

type xDoc struct{ h unsafe.Pointer }

func newDoc() *xDoc { return &xDoc{h: C.fcx_doc_new()} }

func (d *xDoc) free() {
	if d.h != nil {
		C.fcx_doc_free(d.h)
		d.h = nil
	}
}

func (d *xDoc) addTerm(term string) error {
	ct := C.CString(term)
	defer C.free(unsafe.Pointer(ct))
	var cerr *C.char
	if C.fcx_doc_add_term(d.h, ct, &cerr) != 0 {
		return takeErr(cerr)
	}
	return nil
}

func (d *xDoc) addBooleanTerm(term string) error {
	ct := C.CString(term)
	defer C.free(unsafe.Pointer(ct))
	var cerr *C.char
	if C.fcx_doc_add_boolean_term(d.h, ct, &cerr) != 0 {
		return takeErr(cerr)
	}
	return nil
}

type xQuery struct{ h unsafe.Pointer }

func queryWildcard(pattern string) (*xQuery, error) {
	cp := C.CString(pattern)
	defer C.free(unsafe.Pointer(cp))
	var cerr *C.char
	h := C.fcx_query_wildcard(cp, &cerr)
	if h == nil {
		return nil, takeErr(cerr)
	}
	return &xQuery{h: h}, nil
}

func queryTerm(term string) (*xQuery, error) {
	ct := C.CString(term)
	defer C.free(unsafe.Pointer(ct))
	var cerr *C.char
	h := C.fcx_query_term(ct, &cerr)
	if h == nil {
		return nil, takeErr(cerr)
	}
	return &xQuery{h: h}, nil
}

func queryMatchAll() (*xQuery, error) {
	var cerr *C.char
	h := C.fcx_query_match_all(&cerr)
	if h == nil {
		return nil, takeErr(cerr)
	}
	return &xQuery{h: h}, nil
}

const (
	opAnd    = int(C.FCX_OP_AND)
	opOr     = int(C.FCX_OP_OR)
	opAndNot = int(C.FCX_OP_AND_NOT)
)

// queryCombine consumes a and b (they are freed here) and returns the
// combined query.
func queryCombine(op int, a, b *xQuery) (*xQuery, error) {
	defer a.free()
	defer b.free()
	var cerr *C.char
	h := C.fcx_query_combine(C.int(op), a.h, b.h, &cerr)
	if h == nil {
		return nil, takeErr(cerr)
	}
	return &xQuery{h: h}, nil
}

func (q *xQuery) free() {
	if q != nil && q.h != nil {
		C.fcx_query_free(q.h)
		q.h = nil
	}
}

type msetEntry struct {
	docid  uint32
	weight float64
}

func (d *xDB) search(q *xQuery) ([]msetEntry, error) {
	var cerr *C.char
	m := C.fcx_db_search(d.h, q.h, &cerr)
	if m == nil {
		return nil, takeErr(cerr)
	}
	defer C.fcx_mset_free(m)
	n := int(C.fcx_mset_size(m))
	out := make([]msetEntry, 0, n)
	for i := 0; i < n; i++ {
		var w C.double
		docid := C.fcx_mset_docid(m, C.size_t(i), &w)
		out = append(out, msetEntry{docid: uint32(docid), weight: float64(w)})
	}
	return out, nil
}
