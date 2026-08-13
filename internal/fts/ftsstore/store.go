package ftsstore

import (
	"fmt"
	"strings"

	"github.com/yarilomail/yarilo/pkg/fts"
)

// DriverOf returns the storage driver named by an fts_index_root value, and the
// rest of the value with the driver stripped.
//
// The value is written "driver:path", the form namespace locations already use,
// so an operator reads one syntax across the config. A bare path names posix:
// the key shipped without a prefix and deployments already carry values written
// that way.
//
// posix:prefix=/path is the reference form and what a migrated configuration
// carries; posix:/path is the form our namespace locations use. Both name the
// same thing, so both are read.
func DriverOf(root string) (driver, rest string) {
	root = strings.TrimSpace(root)
	name, after, found := strings.Cut(root, ":")
	if !found {
		return "posix", root
	}
	name = strings.ToLower(name)
	if p, ok := strings.CutPrefix(after, "prefix="); ok {
		return name, p
	}
	return name, after
}

// New builds the store named by an fts_index_root value. An unknown driver is
// refused here rather than treated as a path: a name with nothing behind it
// would silently write the index to a directory called after the driver.
//
// storageType is what the operator declared the medium to be
// (fts_storage_type); it means something to posix and is ignored by a driver it
// does not describe.
func New(root string, layout fts.Layout, storageType string) (fts.IndexStore, error) {
	driver, _ := DriverOf(root)
	switch driver {
	case "posix":
		return NewPosix(layout, storageType), nil
	default:
		return nil, fmt.Errorf("fts/store: unknown storage driver %q in fts_index_root (only posix exists)", driver)
	}
}
