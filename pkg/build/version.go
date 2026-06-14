// Package build exposes build-time metadata stamped via -ldflags.
package build

// Version is stamped at build time via -ldflags="-X github.com/0kaba0hub/yarilo/pkg/build.Version=<tag>".
// Falls back to "dev" for local / untagged builds.
var Version = "dev"
