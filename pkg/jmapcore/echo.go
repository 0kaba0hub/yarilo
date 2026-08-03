package jmapcore

import (
	"context"
	"encoding/json"
)

// CoreEcho is RFC 8620 §4: the server returns the arguments it was given. It
// exists so a client can prove the batch machinery — dispatch, ordering,
// back-references — works before any data method is involved.
func CoreEcho(_ context.Context, args json.RawMessage) (any, *MethodError) {
	return args, nil
}

// CoreRegistry is the method set of the core capability.
func CoreRegistry() Registry {
	return Registry{
		"Core/echo": {Capability: CapCore, Fn: CoreEcho},
	}
}
