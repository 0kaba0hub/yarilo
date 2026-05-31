package backendapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/dict"
)

// registerDictRoutes wires the dict admin surface. Every endpoint is
// path-keyed by the dict NAME from cfg.Dicts (e.g. "metadata", "acl",
// "quota") and operates on the live dict.Dict instance handed in via
// Options.Dicts. Wire layouts mirror the pkg/dict contract so the CLI
// (and any future programmatic admin client) is a thin shim.
func (s *Server) registerDictRoutes() {
	s.mux.Handle("GET /api/backend/dict/drivers", s.middleware(s.handleDictDrivers))
	s.mux.Handle("GET /api/backend/dict/{name}/exists", s.middleware(s.handleDictExists))

	s.mux.Handle("POST /api/backend/dict/{name}/lookup", s.middleware(s.handleDictLookup))
	s.mux.Handle("POST /api/backend/dict/{name}/iterate", s.middleware(s.handleDictIterate))
	s.mux.Handle("POST /api/backend/dict/{name}/set", s.middleware(s.handleDictSet))
	s.mux.Handle("POST /api/backend/dict/{name}/unset", s.middleware(s.handleDictUnset))
	s.mux.Handle("POST /api/backend/dict/{name}/atomic-inc", s.middleware(s.handleDictAtomicInc))
	s.mux.Handle("POST /api/backend/dict/{name}/expire-scan", s.middleware(s.handleDictExpireScan))
	s.mux.Handle("POST /api/backend/dict/{name}/commit-batch", s.middleware(s.handleDictCommitBatch))
}

// dictByName returns the live dict.Dict for name, or writes a 404
// response and returns nil on miss. Use as the first line in every
// dict-scoped handler.
func (s *Server) dictByName(w http.ResponseWriter, name string) dict.Dict {
	d, ok := s.opts.Dicts[name]
	if !ok || d == nil {
		apiError(w, fmt.Sprintf("dict %q not configured", name), http.StatusNotFound)
		return nil
	}
	return d
}

// opSettings is the wire shape of pkg/dict.OpSettings. Optional
// fields are omitted (omitempty) so a minimal request stays compact.
type opSettings struct {
	Username   string `json:"username,omitempty"`
	HomeDir    string `json:"home_dir,omitempty"`
	ExpireSecs uint32 `json:"expire_secs,omitempty"`
}

func (o opSettings) toDict() *dict.OpSettings {
	if (opSettings{}) == o {
		return nil
	}
	return &dict.OpSettings{
		Username:   o.Username,
		HomeDir:    o.HomeDir,
		ExpireSecs: o.ExpireSecs,
	}
}

// ----- drivers / exists -----------------------------------------------------

func (s *Server) handleDictDrivers(w http.ResponseWriter, _ *http.Request) {
	out := dict.Drivers()
	sort.Strings(out)
	apiJSON(w, map[string][]string{"drivers": out})
}

func (s *Server) handleDictExists(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	_, ok := s.opts.Dicts[name]
	apiJSON(w, map[string]any{
		"name":   name,
		"exists": ok,
	})
}

// ----- lookup ---------------------------------------------------------------

type lookupReq struct {
	Key string     `json:"key"`
	Op  opSettings `json:"op,omitempty"`
}

type lookupResp struct {
	Found  bool     `json:"found"`
	Values [][]byte `json:"values,omitempty"` // base64 by encoding/json
}

func (s *Server) handleDictLookup(w http.ResponseWriter, r *http.Request) {
	d := s.dictByName(w, r.PathValue("name"))
	if d == nil {
		return
	}
	var req lookupReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Key == "" {
		apiError(w, "key is required", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	vals, found, err := d.Lookup(ctx, req.Op.toDict(), req.Key)
	if err != nil {
		mapDictError(w, err)
		return
	}
	apiJSON(w, lookupResp{Found: found, Values: vals})
}

// ----- iterate (NDJSON streaming) -------------------------------------------

type iterateReq struct {
	Path  string     `json:"path"`
	Flags uint32     `json:"flags,omitempty"` // bitmask of pkg/dict.IterFlag
	Op    opSettings `json:"op,omitempty"`
}

type iterateRow struct {
	Key    string   `json:"key"`
	Values [][]byte `json:"values,omitempty"`
}

func (s *Server) handleDictIterate(w http.ResponseWriter, r *http.Request) {
	d := s.dictByName(w, r.PathValue("name"))
	if d == nil {
		return
	}
	var req iterateReq
	if !decodeJSON(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	it, err := d.Iterate(ctx, req.Op.toDict(), req.Path, dict.IterFlag(req.Flags))
	if err != nil {
		mapDictError(w, err)
		return
	}
	defer it.Close() //nolint:errcheck

	// NDJSON streaming: one row per line, flushed eagerly so clients
	// can pipeline display of large iterations without buffering.
	w.Header().Set("Content-Type", "application/x-ndjson")
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	for it.Next() {
		_ = enc.Encode(iterateRow{Key: it.Key(), Values: it.Values()})
		if flusher != nil {
			flusher.Flush()
		}
	}
	if err := it.Err(); err != nil {
		// Mid-stream error: write a final {"error": msg} object so
		// the CLI surfaces it. The HTTP status is already 200 — the
		// caller MUST check each row for the "error" key.
		_ = enc.Encode(map[string]string{"error": err.Error()})
	}
}

// ----- set / unset / atomic-inc (single-op tx) ------------------------------

type setReq struct {
	Key   string     `json:"key"`
	Value []byte     `json:"value"`
	Op    opSettings `json:"op,omitempty"`
}

type unsetReq struct {
	Key string     `json:"key"`
	Op  opSettings `json:"op,omitempty"`
}

type atomicIncReq struct {
	Key   string     `json:"key"`
	Delta int64      `json:"delta"`
	Op    opSettings `json:"op,omitempty"`
}

type commitResp struct {
	Result int    `json:"result"` // pkg/dict.CommitResult value
	Status string `json:"status"` // "ok" | "not-found" | "failed" | "write-uncertain"
}

func (s *Server) handleDictSet(w http.ResponseWriter, r *http.Request) {
	d := s.dictByName(w, r.PathValue("name"))
	if d == nil {
		return
	}
	var req setReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Key == "" {
		apiError(w, "key is required", http.StatusBadRequest)
		return
	}
	s.runSingleOpTx(w, r, d, req.Op.toDict(), func(tx dict.Tx) error {
		return tx.Set(req.Key, req.Value)
	})
}

func (s *Server) handleDictUnset(w http.ResponseWriter, r *http.Request) {
	d := s.dictByName(w, r.PathValue("name"))
	if d == nil {
		return
	}
	var req unsetReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Key == "" {
		apiError(w, "key is required", http.StatusBadRequest)
		return
	}
	s.runSingleOpTx(w, r, d, req.Op.toDict(), func(tx dict.Tx) error {
		return tx.Unset(req.Key)
	})
}

func (s *Server) handleDictAtomicInc(w http.ResponseWriter, r *http.Request) {
	d := s.dictByName(w, r.PathValue("name"))
	if d == nil {
		return
	}
	var req atomicIncReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Key == "" {
		apiError(w, "key is required", http.StatusBadRequest)
		return
	}
	s.runSingleOpTx(w, r, d, req.Op.toDict(), func(tx dict.Tx) error {
		return tx.AtomicInc(req.Key, req.Delta)
	})
}

// runSingleOpTx wraps a single-op dict.Tx for the trivial endpoints
// (set/unset/atomic-inc). Begin → op → Commit; emits commitResp.
func (s *Server) runSingleOpTx(w http.ResponseWriter, r *http.Request, d dict.Dict, ops *dict.OpSettings, op func(dict.Tx) error) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	tx, err := d.Begin(ctx, ops)
	if err != nil {
		mapDictError(w, err)
		return
	}
	if err := op(tx); err != nil {
		_ = tx.Rollback()
		mapDictError(w, err)
		return
	}
	res, err := tx.Commit()
	if err != nil {
		mapDictError(w, err)
		return
	}
	apiJSON(w, commitResp{Result: int(res), Status: commitStatus(res)})
}

func commitStatus(r dict.CommitResult) string {
	switch r {
	case dict.CommitOK:
		return "ok"
	case dict.CommitNotFound:
		return "not-found"
	case dict.CommitWriteUncertain:
		return "write-uncertain"
	default:
		return "failed"
	}
}

// ----- expire-scan ----------------------------------------------------------

func (s *Server) handleDictExpireScan(w http.ResponseWriter, r *http.Request) {
	d := s.dictByName(w, r.PathValue("name"))
	if d == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	if err := d.ExpireScan(ctx); err != nil {
		mapDictError(w, err)
		return
	}
	apiJSON(w, map[string]string{"status": "ok"})
}

// ----- commit-batch (multi-op atomic) ---------------------------------------

type commitBatchReq struct {
	Op  opSettings `json:"op,omitempty"`
	Ops []batchOp  `json:"ops"`
}

type batchOp struct {
	Kind  string `json:"kind"` // "set" | "unset" | "atomic-inc"
	Key   string `json:"key"`
	Value []byte `json:"value,omitempty"` // base64 for set
	Delta int64  `json:"delta,omitempty"` // for atomic-inc
}

func (s *Server) handleDictCommitBatch(w http.ResponseWriter, r *http.Request) {
	d := s.dictByName(w, r.PathValue("name"))
	if d == nil {
		return
	}
	var req commitBatchReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Ops) == 0 {
		apiError(w, "ops is required and must be non-empty", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	tx, err := d.Begin(ctx, req.Op.toDict())
	if err != nil {
		mapDictError(w, err)
		return
	}
	for i, op := range req.Ops {
		switch strings.ToLower(op.Kind) {
		case "set":
			if op.Key == "" {
				_ = tx.Rollback()
				apiError(w, fmt.Sprintf("ops[%d]: set requires key", i), http.StatusBadRequest)
				return
			}
			if err := tx.Set(op.Key, op.Value); err != nil {
				_ = tx.Rollback()
				mapDictError(w, err)
				return
			}
		case "unset":
			if op.Key == "" {
				_ = tx.Rollback()
				apiError(w, fmt.Sprintf("ops[%d]: unset requires key", i), http.StatusBadRequest)
				return
			}
			if err := tx.Unset(op.Key); err != nil {
				_ = tx.Rollback()
				mapDictError(w, err)
				return
			}
		case "atomic-inc":
			if op.Key == "" {
				_ = tx.Rollback()
				apiError(w, fmt.Sprintf("ops[%d]: atomic-inc requires key", i), http.StatusBadRequest)
				return
			}
			if err := tx.AtomicInc(op.Key, op.Delta); err != nil {
				_ = tx.Rollback()
				mapDictError(w, err)
				return
			}
		default:
			_ = tx.Rollback()
			apiError(w, fmt.Sprintf("ops[%d]: unknown kind %q", i, op.Kind), http.StatusBadRequest)
			return
		}
	}
	res, err := tx.Commit()
	if err != nil {
		mapDictError(w, err)
		return
	}
	apiJSON(w, commitResp{Result: int(res), Status: commitStatus(res)})
}

// ----- error mapping --------------------------------------------------------

// mapDictError translates pkg/dict sentinel errors to the matching
// HTTP status. Generic errors become 500. Currently dict surfaces
// ErrClosed / ErrUnknownDriver and driver-specific wrapped errors.
func mapDictError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, dict.ErrClosed):
		apiError(w, "dict closed", http.StatusServiceUnavailable)
	case errors.Is(err, dict.ErrUnknownDriver):
		apiError(w, err.Error(), http.StatusBadRequest)
	default:
		apiError(w, err.Error(), http.StatusInternalServerError)
	}
}

// Convenience for tests: base64-encode raw bytes for the wire shape.
// Production callers use encoding/json which does this transparently.
func encodeWireBytes(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

var _ = encodeWireBytes // exported via tests
