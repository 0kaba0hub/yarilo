//go:build flatcurve

package ftsservice

import (
	"time"

	"github.com/yarilomail/yarilo/internal/fts/flatcurve"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/fts"
)

func newFlatcurveEngine(cfg config.FTSConfig) (fts.Engine, error) {
	// Validation refused an unknown value at startup, so the fallback here is
	// the unset case, not a silent repair of a typo.
	storageType, _ := config.NormalizeFTSStorageType(cfg.StorageType)
	return flatcurve.New(flatcurve.Options{
		CommitLimit:     cfg.FlatcurveCommitLimit,
		MinTermSize:     cfg.FlatcurveMinTermSize,
		PrefixSearch:    cfg.FlatcurvePrefixSearch,
		OptimizeLimit:   cfg.FlatcurveOptimizeLimit,
		RotateCount:     uint32(cfg.FlatcurveRotateCount),
		RotateTime:      time.Duration(cfg.FlatcurveRotateTimeMsecs) * time.Millisecond,
		SubstringSearch: cfg.FlatcurveSubstringSearch,
		StorageType:     storageType,
	}), nil
}
