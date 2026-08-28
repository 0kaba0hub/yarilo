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
| `mdbox-m.1` | one storage file, three records at offsets 16, 168 and 4690 |
| `sdbox-u.1` | one message, 63-byte body |
| `sdbox-u.2` | one message, 4431-byte body |

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
