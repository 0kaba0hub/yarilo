package dict

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ErrClosed is returned from Dict methods invoked after Close.
// Drivers SHOULD wrap their own context with this sentinel.
var ErrClosed = errors.New("dict: closed")

// ErrUnknownDriver is returned by Open when Config.Driver names a
// driver that was never Registered. Most likely cause: missing
// blank-import of pkg/dict/drivers/all (or the specific driver
// package) in the binary's main package.
var ErrUnknownDriver = errors.New("dict: unknown driver")

// Config selects and configures a dict driver. The Driver field
// names the registered driver (e.g. "file", "redis"); the driver-
// specific configuration lives in Settings, decoded by the driver's
// InitFunc from a generic map.
//
// Keeping all driver-specific knobs under a single map[string]any
// (rather than a typed sub-struct per driver) lets pkg/config stay
// driver-agnostic — adding a new driver does not require editing
// config.go. Driver authors define the schema in their own package
// and decode at Open time.
type Config struct {
	// Driver names the registered driver to instantiate.
	Driver string `koanf:"driver"`
	// Settings is the driver-specific configuration tree. Keys and
	// types are defined by each driver's documentation.
	Settings map[string]any `koanf:"settings"`
}

// InitFunc constructs a Dict from a Config. Drivers register one
// of these via Register at init() time.
type InitFunc func(cfg Config) (Dict, error)

var (
	driversMu sync.RWMutex
	drivers   = map[string]InitFunc{}
)

// Register adds a driver factory under name. It panics on duplicate
// registration so that import-cycle / two-drivers-same-name bugs surface
// at binary startup rather than first use. Driver packages call this
// from their init().
func Register(name string, init InitFunc) {
	if name == "" {
		panic("dict: Register called with empty driver name")
	}
	if init == nil {
		panic("dict: Register called with nil init for " + name)
	}
	driversMu.Lock()
	defer driversMu.Unlock()
	if _, dup := drivers[name]; dup {
		panic("dict: driver already registered: " + name)
	}
	drivers[name] = init
}

// Drivers returns the sorted list of currently-registered driver
// names. Used by yarctl to print the available choices when
// the user passes an unknown driver name.
func Drivers() []string {
	driversMu.RLock()
	defer driversMu.RUnlock()
	out := make([]string, 0, len(drivers))
	for name := range drivers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Open instantiates the driver named cfg.Driver with the supplied
// settings. Returns ErrUnknownDriver if the driver was not Registered.
func Open(cfg Config) (Dict, error) {
	if cfg.Driver == "" {
		return nil, fmt.Errorf("dict: Config.Driver is empty")
	}
	driversMu.RLock()
	init, ok := drivers[cfg.Driver]
	driversMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w %q (registered: %v)", ErrUnknownDriver, cfg.Driver, Drivers())
	}
	d, err := init(cfg)
	if err != nil {
		return nil, fmt.Errorf("dict/%s: %w", cfg.Driver, err)
	}
	return d, nil
}
