//go:build !flatcurve

package ftsservice

import (
	"fmt"

	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/fts"
)

func newFlatcurveEngine(_ config.FTSConfig) (fts.Engine, error) {
	return nil, fmt.Errorf("ftsservice: the flatcurve engine is not built into this binary (build with -tags flatcurve; the yarilo-fts image carries it)")
}
