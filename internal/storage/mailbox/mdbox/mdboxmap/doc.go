// Package mdboxmap implements the per-user mdbox map (`yarilo.map.index`): one
// record per stored message under a map_uid every folder index references and no
// storage file carries, so the map is not derivable from the messages on disk.
// It is what makes COPY O(1). On-disk layout, log format and locking model:
// INTERNALS.md in docs-internal.
package mdboxmap
