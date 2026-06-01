// yarilo-migrate converts a per-user mailbox tree from one
// storage shape to another. Supported source shapes:
//
//	maildir   — Maildir++ with cur/, dovecot-uidlist, etc.
//	dbox-v1   — pre-Phase-5 yarilo single-message dbox
//	mdbox-v1  — pre-Phase-5 yarilo multi-message dbox (TSV dbox.map)
//
// Supported destination shapes (Phase 6+):
//
//	sdbox     — Dovecot-compliant single-message dbox driver
//	mdbox     — Dovecot-compliant multi-message dbox driver
//
// Usage:
//
//	yarilo-migrate --src dbox-v1 --dst sdbox --from /var/legacy --to /var/yarilo
//	yarilo-migrate --src maildir --dst mdbox --from /var/maildir --to /var/yarilo
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	indexfile "github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/mdbox"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

var (
	flagSrc    = flag.String("src", "", "source format: maildir | dbox-v1 | mdbox-v1 (required)")
	flagDst    = flag.String("dst", "", "destination format: sdbox | mdbox (required)")
	flagFrom   = flag.String("from", "", "source root (required)")
	flagTo     = flag.String("to", "", "destination root (required)")
	flagDry    = flag.Bool("dry-run", false, "print actions without writing")
	flagFormat = flag.String("format", "", "[deprecated] alias for --dst")
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	flag.Parse()

	if *flagSrc == "" {
		// Back-compat: if neither --src nor --dst supplied, the
		// pre-Phase-7 invocation was `--format <dst>` over a
		// Maildir tree. Honour it.
		*flagSrc = "maildir"
	}
	if *flagDst == "" && *flagFormat != "" {
		*flagDst = *flagFormat
	}
	if *flagFrom == "" || *flagTo == "" || *flagDst == "" {
		fmt.Fprintln(os.Stderr,
			"usage: yarilo-migrate --src <maildir|dbox-v1|mdbox-v1> --dst <sdbox|mdbox> --from <src> --to <dst>")
		os.Exit(1)
	}

	walker, err := pickWalker(*flagSrc)
	if err != nil {
		slog.Error("source format", "err", err)
		os.Exit(1)
	}
	box, err := pickBackend(*flagDst)
	if err != nil {
		slog.Error("destination format", "err", err)
		os.Exit(1)
	}
	idx := indexfile.New()
	resolver := &mailbox.Resolver{Root: *flagTo, HomeTemplate: "%d/%n"}

	var migrated, skipped int
	err = filepath.WalkDir(*flagFrom, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(*flagFrom, path)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 2 || !d.IsDir() {
			return nil
		}
		user := parts[1] + "@" + parts[0]
		m, s, err := migrateUser(walker, *flagFrom, box, idx, resolver, user)
		migrated += m
		skipped += s
		return err
	})
	if err != nil {
		slog.Error("migration failed", "err", err)
		os.Exit(1)
	}
	slog.Info("migration complete", "migrated", migrated, "skipped", skipped, "src", *flagSrc, "dst", *flagDst)
}

func pickBackend(dst string) (mailbox.MailboxBackend, error) {
	switch strings.ToLower(dst) {
	case "sdbox", "dbox":
		return dboxv2.New(), nil
	case "mdbox":
		return mdbox.New(), nil
	default:
		return nil, fmt.Errorf("unknown --dst %q (want sdbox|mdbox)", dst)
	}
}

func migrateUser(walker sourceWalker, srcRoot string, boxBE mailbox.MailboxBackend, idxBE mailbox.IndexBackend, resolver *mailbox.Resolver, user string) (migrated, skipped int, _ error) {
	srcHome := userDir(srcRoot, user)
	info := resolver.UserInfo(user, "")
	box := boxBE.OpenUser(info)
	defer box.Close() //nolint:errcheck
	idx := idxBE.OpenUser(info)
	defer idx.Close() //nolint:errcheck

	if err := box.Init(); err != nil {
		return 0, 0, fmt.Errorf("migrate: init %s: %w", user, err)
	}

	createdFolders := map[string]bool{"INBOX": true}
	folders := map[string]*mailbox.Folder{}

	err := walker.Walk(srcHome, func(msg sourceMessage) error {
		// Lazy folder create + index OpenFolder.
		if !createdFolders[msg.Folder] {
			if err := box.Create(msg.Folder); err != nil {
				return fmt.Errorf("create %s/%s: %w", user, msg.Folder, err)
			}
			createdFolders[msg.Folder] = true
		}
		f, ok := folders[msg.Folder]
		if !ok {
			ff, err := idx.OpenFolder(msg.Folder, uint32(os.Getpid()))
			if err != nil {
				return fmt.Errorf("openfolder %s/%s: %w", user, msg.Folder, err)
			}
			folders[msg.Folder] = ff
			f = ff
		}

		if *flagDry {
			slog.Info("would migrate", "user", user, "folder", msg.Folder, "size", len(msg.Body))
			skipped++
			return nil
		}

		uid, err := idx.AllocateUID(f.ID)
		if err != nil {
			return fmt.Errorf("allocate %s/%s: %w", user, msg.Folder, err)
		}
		filename, err := box.Save(msg.Folder, msg.bodyReader(), uid, int64(len(msg.Body)), msg.Flags)
		if err != nil {
			return fmt.Errorf("save %s/%s: %w", user, msg.Folder, err)
		}
		meta := &mailbox.MessageMeta{
			UID:          uid,
			Filename:     filename,
			Flags:        msg.Flags,
			Size:         uint32(len(msg.Body)),
			InternalDate: msg.InternalDate,
			GUID:         msg.GUID,
		}
		if err := idx.AppendMessage(f.ID, meta); err != nil {
			return fmt.Errorf("append %s/%s: %w", user, msg.Folder, err)
		}
		migrated++
		return nil
	})
	if err != nil {
		return migrated, skipped, err
	}
	return migrated, skipped, nil
}

// userDir maps user@domain → <root>/<domain>/<localpart>. Used
// when iterating the source tree to find per-user homes.
func userDir(root, user string) string {
	if at := strings.LastIndex(user, "@"); at >= 0 {
		return filepath.Join(root, user[at+1:], user[:at])
	}
	return filepath.Join(root, user)
}

// maildirFlags parses Maildir flag chars from filename ":2,<flags>".
// Kept here (not in source.go) so source.go has no Maildir-only
// awareness — pickWalker stays the only place that maps strings
// to types.
func maildirFlags(filename string) []string {
	idx := strings.Index(filename, ":2,")
	if idx < 0 {
		return nil
	}
	var flags []string
	for _, c := range filename[idx+3:] {
		switch c {
		case 'D':
			flags = append(flags, `\Draft`)
		case 'F':
			flags = append(flags, `\Flagged`)
		case 'R':
			flags = append(flags, `\Answered`)
		case 'S':
			flags = append(flags, `\Seen`)
		case 'T':
			flags = append(flags, `\Deleted`)
		}
	}
	return flags
}
