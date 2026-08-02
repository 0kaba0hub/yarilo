//go:build flatcurve

package ftsservice

import (
	"time"

	"github.com/yarilomail/yarilo/internal/fts/flatcurve"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/fts"
)

func newFlatcurveEngine(cfg config.FTSConfig) (fts.Engine, error) {
	return flatcurve.New(flatcurve.Options{
		CommitLimit:     cfg.FlatcurveCommitLimit,
		MinTermSize:     cfg.FlatcurveMinTermSize,
		OptimizeLimit:   cfg.FlatcurveOptimizeLimit,
		RotateCount:     uint32(cfg.FlatcurveRotateCount),
		RotateTime:      time.Duration(cfg.FlatcurveRotateTimeMsecs) * time.Millisecond,
		SubstringSearch: cfg.FlatcurveSubstringSearch,
	}), nil
}
