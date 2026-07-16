package ftsservice

import (
	"fmt"

	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/fts"
)

// BuildEngine resolves the configured fts_engine. The engine is required and
// explicit: no name (or an unknown one) fails startup fast, so the active
// engine is always stated in config.
func BuildEngine(cfg config.FTSConfig) (fts.Engine, error) {
	switch cfg.Engine {
	case "flatcurve":
		return newFlatcurveEngine(cfg)
	case "bleve":
		return nil, fmt.Errorf("ftsservice: engine %q arrives with its own stream", cfg.Engine)
	case "":
		return nil, fmt.Errorf("ftsservice: fts_engine is required when fts is enabled")
	default:
		return nil, fmt.Errorf("ftsservice: unknown fts_engine %q", cfg.Engine)
	}
}
