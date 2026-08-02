// Package all blank-imports every in-tree dict driver so a single
// import enables all of them in the importing binary.
//
//	import _ "github.com/yarilomail/yarilo/pkg/dict/drivers/all"
//
// Drivers self-register via init() in their own packages; this file
// just pulls them in. Binaries that want a leaner footprint can
// import individual driver packages instead and skip the ones they
// do not need (config-not-binary rule is preserved either way — the
// CHOICE of driver is still config, not compile-time; we are only
// controlling which drivers are eligible to be chosen).
package all

import (
	_ "github.com/yarilomail/yarilo/pkg/dict/fail"
	_ "github.com/yarilomail/yarilo/pkg/dict/file"
	_ "github.com/yarilomail/yarilo/pkg/dict/memory"
	_ "github.com/yarilomail/yarilo/pkg/dict/redis"
	_ "github.com/yarilomail/yarilo/pkg/dict/sql"
)
