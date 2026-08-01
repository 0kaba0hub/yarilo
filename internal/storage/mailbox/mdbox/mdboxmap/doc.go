// Package mdboxmap implements the global per-user multi-message
// dbox map (on-disk file: `yarilo.map.index`). It is the
// keystone of the mdbox storage model:
//
//   - one binary mailindex file shared by every folder in the
//     user's mdbox tree
//   - each record describes one stored message:
//     (file_id, offset, size) under the "map" extension
//     plus a uint16 refcount under the "ref" extension
//   - the mailindex Record.UID column is the message's "map_uid",
//     a globally-unique handle that every folder-index references
//   - the map header carries `highest_file_id` — the highest m.<N>
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
//	header     standard mailindex header
//	exts       "map" (12 B per-record, 4 B header for highest_file_id)
//	           "ref" (2 B per-record, atomic-inc)
//	records    Record{UID = map_uid, Ext["map"] = {file_id, offset, size},
//	                  Ext["ref"] = refcount}
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
