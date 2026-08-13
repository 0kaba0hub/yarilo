//go:build flatcurve

package ftsservice

import (
	"time"

	"github.com/yarilomail/yarilo/internal/fts/flatcurve"
	"github.com/yarilomail/yarilo/internal/fts/ftsstore"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/fts"
)

func newFlatcurveEngine(cfg config.FTSConfig) (fts.Engine, error) {
	// Validation refused an unknown value at startup, so the fallback here is
	// the unset case, not a silent repair of a typo.
	storageType, _ := config.NormalizeFTSStorageType(cfg.StorageType)
	// fts_index_root names the store; the engine is handed one and never
	// learns which medium it is (#1053). An unknown driver is refused here,
	// where the engine is built, rather than surfacing later as an index
	// written into a directory named after the driver.
	store, err := ftsstore.New(cfg.IndexRoot, flatcurve.Layout(), storageType)
	if err != nil {
		return nil, err
	}
	return flatcurve.New(flatcurve.Options{
		CommitLimit:     cfg.FlatcurveCommitLimit,
		MinTermSize:     cfg.FlatcurveMinTermSize,
		PrefixSearch:    cfg.FlatcurvePrefixSearch,
		OptimizeLimit:   cfg.FlatcurveOptimizeLimit,
		RotateCount:     uint32(cfg.FlatcurveRotateCount),
		RotateTime:      time.Duration(cfg.FlatcurveRotateTimeMsecs) * time.Millisecond,
		SubstringSearch: cfg.FlatcurveSubstringSearch,
		Store:           store,
	}), nil
}
