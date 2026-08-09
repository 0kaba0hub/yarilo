// Package mdboxmap implements the global per-user multi-message
// dbox map (on-disk file: `yarilo.map.index`). It is the
// keystone of the mdbox storage model:
//
//   - one binary file shared by every folder in the user's mdbox tree
//   - each record describes one stored message:
//     (file_id, offset, size), a uint16 refcount and the message GUID
//   - the record's UID column is the message's "map_uid", a
//     globally-unique handle that every folder-index references. It
//     exists nowhere else: no storage file carries it, so the map is
//     not derivable from the messages on disk
//   - the header carries `highest_file_id` — the highest m.<N>
//     physical file ever allocated under this map
//
// The map is what makes IMAP COPY O(1) in mdbox: a copy writes
// one new per-folder record carrying the source's map_uid and
// bumps the refcount; zero bytes of body are read or copied.
// Purge later sweeps records whose refcount has dropped to zero.
//
// This package is consumed by the (forthcoming Phase 5) mdbox
// driver rewrite. It is intentionally storage-driver-agnostic:
// nothing here knows about folder layout or per-folder indexes.
//
// Wire layout — see the internal docs for the byte-level spec:
//
//	header     64 B, fixed: magic, version, counters, and how far into
//	           the append log the records below already reach
//	records    36 B each, sorted by map_uid: {map_uid, file_id, offset,
//	           size, refcount, guid}
//
// Fixed-width and sorted is what makes an open cheap: a record is
// addressed by offset arithmetic and found by binary search over the
// bytes, so there is nothing to parse and no lookup table to build.
// The version byte is there to refuse, not to adapt — a base this
// binary does not recognise is never guessed at, because the map
// decides which bytes belong to which message and which file a purge
// may unlink.
//
// The append log is a separate format and keeps its own: transactions
// carry mailindex records under a layout fixed in this package.
//
// Locking model:
//
//   - cross-process: the user-wide map atomic lock — held during
//     map_uid allocation, file_id assignment, refcount updates,
//     and purge moves
//   - in-process: a sync.Mutex inside Map for cheap re-entrant
//     access from the same goroutine
//
// Caller order when saving + assigning map_uids:
//
//	batch := m.AppendBatch()
//	for each new message {
//	    fileID, offset := batch.Next(size)
//	    // write body to <storage>/m.<fileID> at offset
//	}
//	mapUIDs, err := batch.Finish()  // bumps highest_file_id,
//	                                // assigns map_uids, persists
package mdboxmap
