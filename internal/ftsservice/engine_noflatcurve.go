//go:build !flatcurve

package ftsservice

import (
	"fmt"

	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/fts"
)

func newFlatcurveEngine(_ config.FTSConfig) (fts.Engine, error) {
	return nil, fmt.Errorf("ftsservice: the flatcurve engine is not built into this binary (build with -tags flatcurve; the yarilo-fts image carries it)")
}
