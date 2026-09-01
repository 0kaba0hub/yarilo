# Reference dbox v2 fixtures

Files written by a reference dbox v2 implementation, version 2.4.4, and read
back by our drivers' tests. They are here so that the interoperability claim in
`STORAGE.md` rests on bytes another implementation produced rather than on
bytes ours produced.

## How they were made

A local install of the reference implementation, no daemon: mail was delivered
with its `save` command against a minimal configuration that set only the
storage driver, the mail path, and a passwd-file user. Nothing else was
configured, so nothing about the layout below depends on tuning.

```
mail_driver = mdbox        # sdbox for the u.* files
mail_path   = <home>/mdbox
```

| file | what it is |
|---|---|
| `mdbox-m.1` | one storage file, three records at offsets 16, 168 and 4690 (see below) |
| `sdbox-u.1` | one message, 63-byte body |
| `sdbox-u.2` | one message, 4431-byte body |

## Where the offsets come from

The three offsets are not something our reader worked out. They are what the
reference itself recorded for this file, read back out of the map index it
wrote beside it:

```
dump <store>/storage/<map index>
...
RECORD: seq=1 ... offset = 16    size = 152
RECORD: seq=2 ... offset = 168   size = 4522
RECORD: seq=3 ... offset = 4690  size = 159
```

(The sizes there are whole records — header, body and trailer. The body sizes
the tests assert come from each record's own size field, which the reference
also wrote.)

**Rule for re-taking these fixtures: read the offsets from there again, never
from our reader.** A number that came out of the implementation under test is
not a check on it, and a test repaired by copying whatever our parser now
reports has been quietly turned into a tautology. The numbers are constants in
the test on purpose -- a re-taken fixture should break it loudly -- and this is
the only legitimate place to get the replacements.

## What each one covers

**`mdbox-m.1` — records that are not preceded by a file header.** Only the
first record follows the `2 M1e C…` line. Reading the second and third is the
path that must take the message-header size from the file header, because
there is none in front of them; a reader carrying a constant instead gets a
body shifted by two bytes and a trailer that does not parse.

**Bodies past the peek window.** Record 2 is 4430 bytes and `sdbox-u.2` is
4431 bytes, both well past the 64-byte window a reader looks at to find the
record header. A fixture that fit inside one read would not distinguish a
reader that walks the file from one that got lucky.

**A folder other than INBOX.** Record 3 was saved to `Archive`, so its trailer
carries `BArchive` where the other two carry `BINBOX`.

Two things about `B` that these fixtures cannot show, because every folder in
them is plain ASCII and mdbox:

- it is the **storage** name, not the one a client sees. The reference writes
  `box->name` (`mdbox-save.c`), which is modified UTF-7 for anything not plain
  ASCII, so `Вхідні/Робота` appears as `&BBIERQRWBDQEPQRW-/&BCAEPgQxBD4EQgQw-`.
  For `Archive` and `INBOX` the encoded and decoded forms are the same string,
  which is exactly why a reader that skipped the decoding passes on these
  files;
- **sdbox does not write it at all** (`sdbox-save.c` passes NULL). A
  single-message file already sits in its folder's directory, so it needs no
  hint -- and a recovery that depends on `B` is an mdbox thing only.

Two things the reference settled that we had guessed at:

- the trailer keys come in the order `R`, `V`, `G`, `B` — our hand-built
  fixtures used `G`, `R`, `V`;
- `B` is written for INBOX too, not only for other folders. A record *without*
  `B` is therefore not something this implementation produces, and the row for
  it stays a hand-built one.

## Direction two: our bytes against these

There is no reference process reading our files in these tests. What is
checked instead is that a record we write agrees with one of these — but not
uniformly, because only part of a record is positional.

The file-header line and the message header are compared **byte for byte**,
apart from the create stamp: every byte of them has a fixed place, and they are
what the other implementation reads before it appends.

The trailer is compared as **the same set of keys and the same values**, not as
bytes. It is keyed lines, and the two do not write them in the same order —
`R`, `V`, `G`, `B` there against `G`, `R`, `V`, `B` here. Comparing the bytes
would assert an order the format does not require and that our writer does not
produce.

That is a proxy, and it is worth naming as one. It says the reference will
parse our record by construction, because our record *is* its record; it does
not say the reference accepted our **store** — the map file, the refcounts, the
indexes. That is a different question and it is #1524.

## What the reference read for real, once

The proxy is not the only thing that was done. A file our sdbox writer produced
was placed into a store belonging to the reference implementation, its indexes
dropped, and its own fetch command run against it. It returned the headers and
the body in full.

That was a one-off by hand, not something CI repeats, and it needed the file
renamed: the reference names a single-message file after the UID, `u.1`, while
we name it after the GUID. Renaming is a store-level difference, which is the
same boundary as everywhere else here -- the record was read as written.

Two things that came out of doing it:

- **`V` diverges.** On the fixture's third record the reference writes `V48`
  and we write `V43`. The key holds the CRLF-counted size; we put the length of
  the bytes on disk into it. Real, and tracked in #1527 -- so the byte
  comparison in the tests leaves `V` out and says why, rather than restating it
  as a failing row.
- **The trailer order and the `B` key**, as above.

The same reasoning covers appending: the check the reference makes before it
appends to an existing file is on the first line, `version == 2` and `M == 30`.
Our file-header line matches these byte for byte apart from the create stamp,
so that check passes. There is a test that says exactly this and nothing wider.

---

# Index fixtures (#1524)

A second store, captured for the import work. The record fixtures above are
about the *format of one message*; these are about the *state of a mailbox* --
which messages exist, what flags they carry, and where that state actually
lives.

| file | what it is |
|---|---|
| `index-inbox.index` | the base index of INBOX, 704 bytes |
| `index-inbox.log` | the current transaction log |
| `index-inbox.log.2` | the rotated one |
| `map.log` | the mdbox map's transaction log |
| `store-m.1` | the storage file the index above describes |

## The state they carry

Four messages in INBOX and one in a second folder:

| uid | state |
|---|---|
| 1 | `\Seen` |
| 2 | `\Answered` |
| 3 | keyword `$Important` |
| 4 | **expunged** |
| 5 | nothing |
| Archive, uid 1 | one message in a folder that is not INBOX |

Each row is there to fail something: a reader that ignores flag updates keeps 1
and 2 plain, one that cannot read the keyword extension loses 3, one that
replays appends without expunges brings 4 back, and one that assumes INBOX
misses the Archive message entirely.

**The log carries two transactions taken after that fetch was recorded**: uid 6
is appended and uid 5 is expunged, so the live set these files describe is
1, 2, 3, 6 with `next_uid` 7. That line is derived from the bytes rather than
from their fetch -- the store it came from is gone, so it cannot be re-read the
way the rest of this file was, and it is marked here rather than quietly folded
into the table above.

## Two things the capture established

**The log alone is not the state, and neither is the base.** `index-inbox.log.2`
exists because the log rotated: what it held is in the base now, and what came
after is in `index-inbox.log`. A reader that takes only the log loses everything
older than the rotation; one that takes only the base loses everything newer
than `log_file_tail_offset`.

**The map has no base here, and that is a state rather than a rule.**
`dovecot.map.index` is absent from this store and from the earlier one. It is
not that the reference never writes it: `mail-index-sync.c` rewrites the base
once the log read since the last rewrite passes `rewrite_max_log_bytes`, or once
the header points at a rotated-away `.log`. This store's map log has not rotated
-- there is no `map.log.2` -- and eight hundred transactions is a count, not a
size.

After a rotation the intros in a new map log carry only extension ids, having
been named in a file that no longer exists; the ids then refer to the table in
`dovecot.map.index`. The reader takes that table as a seed and refuses an id
nothing introduced, rather than skipping it -- skipping returns a map that is
silently short, which is a mailbox whose messages point at nothing. **The
rotated case has no fixture yet**: this store's map log has not rotated, so the
seeded path is written and unexercised.

So the map branch has to handle **both**: a base plus the log from
`log_file_tail_offset` when `dovecot.map.index` exists, exactly as for a folder,
and the log from the beginning when it does not. This fixture covers the second
case only. The first arrives when the map is driven through a rotation, the same
way INBOX was here.

## How they were made

The same local install of the reference implementation, version 2.4.4, driven
through its `save`, `flags` and `expunge` commands with no daemon.

Two settings were changed from their defaults, and only to make the rotation
happen in seconds instead of at the end of a working day:

```
mail_index_log_rotate_min_age = 0
mail_index_log_rotate_max_size = 20 k
```

The rotation was forced by toggling `\Flagged` on uid 5 seven hundred times.
An earlier attempt added seven hundred *keywords* instead, which rotated the log
just as well and left a keyword table larger than everything else in the file --
a fixture whose shape said more about how it was made than about what it is.
The thresholds are timing knobs; nothing about the format on disk depends on
them.

**Re-taking these:** the state above is the fixture. Read it back with the
reference's own `fetch` command, as it was recorded here, and never from our
reader -- same rule as the record offsets above.

---

# A folder with no base index (#1564)

| file | what it is |
|---|---|
| `index-fresh.log` | a folder's transaction log, with no base index beside it |

Three messages in a folder `Fresh`, and nothing has forced a base index to be
written yet. This is not a contrived state: the reference writes a base only
once a size threshold or a rotation makes it, so every folder of a freshly
created store looks like this, and a folder made a moment ago looks like this on
a live one.

## The state it carries

What the reference's own `fetch` reports over this folder, verbatim:

```
uid: 1
flags: \Seen \Recent

uid: 2
flags: \Answered \Recent

uid: 3
flags: \Recent $Important
```

(`\Recent` is session state, not stored state, and no import carries it.)

Each row fails something different: a reader that takes `log_file_tail_offset`
as the starting point -- the base's rule, applied where there is no base --
returns nothing at all; one that treats a missing base as a missing index sends
the folder to the store scan and delivers all three without flags.

## What it establishes beyond the flags

The extension a message's bytes are found through is **named** in this log. The
reference writes an intro by name while the index map does not yet know the
extension, and by id once it does (`log_append_ext_intro`), so the first intro
for each extension in a folder's first log necessarily carries its name. That is
why "the log from its start" needs no base to interpret it, and why "base plus
log" does: after a rotation the new log's intros are ids referring to a table
that lives in the base.

## How it was made

The same local install, no daemon, defaults throughout -- no rotation knobs,
because the point of this fixture is a log that has **not** rotated:

```
doveadm mailbox create Fresh
doveadm save -m Fresh            (three times)
doveadm flags add '\Seen'      mailbox Fresh uid 1
doveadm flags add '\Answered'  mailbox Fresh uid 2
doveadm flags add '$Important' mailbox Fresh uid 3
```

Then `dbox-Mails/dovecot.index.log` was taken, after checking that
`dovecot.index` had not appeared beside it.

**Re-taking this:** check for the absence of the base file first -- with one
there, the fixture silently becomes an ordinary folder and the test it guards
passes for the wrong reason. Read the state back with the reference's `fetch`,
never from our reader.


---

# A folder whose tail was expunged (#1568)

| file | what it is |
|---|---|
| `index-tail.log` | a folder's log: three messages saved, the last expunged |
| `map-tail.log` | that store's map log |
| `store-tail-m.1` | the storage file the two describe |

## The state it carries

The reference's own output, verbatim:

```
doveadm fetch 'uid flags' mailbox Tail
uid: 1
flags: \Seen \Recent

uid: 2
flags: \Recent

doveadm mailbox status "uidnext messages uidvalidity" Tail
Tail messages=2 uidnext=4 uidvalidity=1788118011
```

## What it establishes

**`next_uid` cannot be derived from the messages.** The highest surviving uid is
2 and the counter is at 4, because uid 3 was appended -- which moved the counter
-- and then expunged, which left no record behind. The reference does not
journal the counter: it moves `next_uid` past each uid as it applies the append
(`mail-index-sync-update.c`). So a reader that takes the highest surviving uid
plus one hands the next delivery a number a client has already seen carrying
different mail.

The other index fixture cannot show this: there the highest surviving uid
happens to be the highest ever used, so both readings agree by luck.

It is also a folder with no base index, so it exercises the log-only branch at
the same time.

## How it was made

The same local install, no daemon, defaults throughout:

```
doveadm mailbox create Tail
doveadm save -m Tail                       (three times)
doveadm flags add '\Seen' mailbox Tail uid 1
doveadm expunge mailbox Tail uid 3
```

**Re-taking this:** the gap between `uidnext` and the highest surviving uid is
the fixture. Read both back with the reference's own `status` and `fetch`, never
from our reader, and check the gap is still there -- without it the test it
guards passes for the wrong reason.


---

# A map with a base (#1583)

| file | what it is |
|---|---|
| `map-based.index` | the map's base index, 718 records |
| `map-based.log` | its transaction log |

Seven hundred and sixty messages, of which the base holds 718 and the log the
rest: the reference folds the log into the base once enough of it has been read,
and keeps writing to the log afterwards. So **neither half is the map**, and a
reader taking one of them is short by the other.

The reference's own count over the store this came from:

```
doveadm mailbox status "messages uidnext" INBOX
INBOX messages=760 uidnext=761
```

## What it establishes

The rule for a map is the rule for a folder, which is what the reader's own doc
said and what nothing had checked: base records first, then the log from
`log_file_tail_offset`.

Read from the log alone, this fixture still comes out at 760 -- its log has not
rotated, so it happens to hold everything. That is why the test also asserts the
two halves separately: 718 in the base, fewer than 760 past the tail. A reader
that dropped the base reads 42.

**A rotated map log still has no fixture.** Rotation could not be forced on this
install -- the folder logs rotate under the size knobs and the map's does not --
and that is the case where taking the log alone loses messages outright rather
than by accident. It was reached in the field instead: a store of three thousand
messages, where the conversion stopped on a folder naming a map uid the log no
longer described.

## How it was made

The same local install, no daemon, 760 messages saved into INBOX with
`mail_index_log_rotate_min_age = 0` and `mail_index_log_rotate_max_size = 20 k`
-- the knobs that make the base appear in seconds rather than at the end of a
working day. Nothing about the format depends on them.

**Re-taking this:** the gap between the base's record count and the message
count is the fixture. Read both back with the reference's own `status`, and
check the base is still short -- with the two equal, the test it guards passes
for the wrong reason.


---

# A map whose log has rotated (#1583, #1587)

| file | what it is |
|---|---|
| `map-rotated.index` | the map's base after a rotation: every record is here |
| `map-rotated.log` | the current log, a bare 40-byte header |

Three thousand and twenty messages, all of them in the base. The reference's own
count over the store this came from:

```
doveadm mailbox status "messages uidnext" INBOX
INBOX messages=3020 uidnext=3021
```

## What it establishes

Reading the log alone returns **nothing at all** — not a short map, an empty
one. Every folder naming a map uid then fails to convert, and the mailbox is
unavailable in full. That is what the field hit before any fixture could show
it, and it is the strongest input for the rule that a map is its base plus its
log.

## What it does not establish, and why

**The sequence numbers agree here.** The base says `log_file_seq = 3` and the
current log is sequence 3; the rotated one is 2. The store was rotated by the
reference's own resync, which rewrites the base at the same time, so the base is
never left pointing at the file the rotation moved away.

So this fixture does not exercise the check in #1589 -- a base whose tail offset
belongs to a log that is no longer on disk. That test keeps its constructed
input: the fixture's own base with one header field moved to a sequence its log
does not carry. Said here rather than implied, because a fixture that looks like
it covers a case it does not is worse than no fixture.

## What was left out

The store also had a 423 KB `dovecot.map.index.log.2`. It is not here: on this
path nothing reads it -- the sequences agree, so the rotated log is never
consulted -- and carrying it would triple this directory for bytes no test
touches. If a rotated-away log ever needs a fixture, it needs one whose base
points at it, which this store is not.

## How it was made

Captured from a sandbox store of 3020 messages after `doveadm force-resync`,
which is what rotated the log. Not reproducible on the local install: the folder
logs rotate under the size knobs and the map's does not.

## The sdbox fixtures

`sdbox-inbox.log`, `sdbox-cyrillic.log` and `sdbox-inbox-u.1` … `u.4` were taken
from a store the reference wrote, on 2.4.5, with `mail_driver = sdbox` and a
`mail_index_path` of its own — so the index sits under its own root and the
messages stay with the mail, which is the shape a deployment with `INDEX=` set
produces.

The store: five messages saved to INBOX, `\Seen` on uid 1, `\Answered` on uid 2,
`$Important` on uid 3, `$Important $Label` on uid 4, and uid 5 expunged. A
Cyrillic folder and a folder inside it, both subscribed, INBOX not.

What its own server reported for that folder, which is the oracle the tests
assert against:

```
uidvalidity=1788252508 uidnext=6 messages=4
uid 1  \Seen                      c2d304036291966a4a0000000a4d75c4
uid 2  \Answered                  692555046291966a4d0000000a4d75c4
uid 3  $Important                 e13e86056291966a510000000a4d75c4
uid 4  $Important $Label          c919ef066291966a540000000a4d75c4
```

Three things these files settle that a constructed input would only have
confirmed about our own reading:

**The message files are named by uid**, `u.1` … `u.4`, not by guid. Our own
driver names new files `u.<guidhex>`, so the two spellings meet in one folder
after a conversion.

**Their sdbox folder index has no `guid` extension.** Its extensions are
`dbox-hdr`, `hdr-pop3-uidl`, `cache`, `vsize` and `hdr-vsize`. The guid is in
each message file's trailer, which is where their own server reads it, and a
conversion that expected it in the index would write four zero guids — a fresh
index of ours is marked guid-complete, so nothing would ever come back to fill
them.

**There is no base index at all**, only `dovecot.index.log`: a folder the
reference has written but not yet folded.

To re-take: build the store the same way, `doveadm fetch 'uid flags guid'` and
`doveadm mailbox status` with the server stopped, and update the oracle above
together with the numbers in the tests.
