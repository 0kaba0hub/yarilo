// yarilo-migrate converts a Maildir tree to sdbox or mdbox format in-place.
// Usage: yarilo-migrate --from /var/mail/vhosts --to /var/mail/sdbox --format sdbox|mdbox [--dry-run]
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/mdbox"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

var (
	flagFrom   = flag.String("from", "", "source Maildir root (required)")
	flagTo     = flag.String("to", "", "destination root (required)")
	flagFormat = flag.String("format", "sdbox", "output format: sdbox or mdbox (alias: dbox=sdbox)")
	flagDry    = flag.Bool("dry-run", false, "print actions without writing")
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	flag.Parse()

	if *flagFrom == "" || *flagTo == "" {
		fmt.Fprintln(os.Stderr, "usage: yarilo-migrate --from <src> --to <dst> --format sdbox|mdbox")
		os.Exit(1)
	}

	var box mailbox.MailboxBackend
	switch strings.ToLower(*flagFormat) {
	case "sdbox", "dbox":
		box = dboxv2.New()
	case "mdbox":
		box = mdbox.New()
	default:
		slog.Error("unknown format", "format", *flagFormat)
		os.Exit(1)
	}
	idx := file.New()

	resolver := &mailbox.Resolver{
		Root:         *flagTo,
		HomeTemplate: "%d/%n",
	}

	var migrated, skipped int
	err := filepath.WalkDir(*flagFrom, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(*flagFrom, path)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 2 || !d.IsDir() {
			return nil
		}
		user := parts[1] + "@" + parts[0]
		m, s, err := migrateUser(*flagFrom, box, idx, resolver, user)
		migrated += m
		skipped += s
		return err
	})
	if err != nil {
		slog.Error("migration failed", "err", err)
		os.Exit(1)
	}
	slog.Info("migration complete", "migrated", migrated, "skipped", skipped)
}

func migrateUser(srcRoot string, boxBE mailbox.MailboxBackend, idxBE mailbox.IndexBackend, resolver *mailbox.Resolver, user string) (migrated, skipped int, _ error) {
	userPath := userDir(srcRoot, user)
	info := resolver.UserInfo(user, "")
	box := boxBE.OpenUser(info)
	defer box.Close() //nolint:errcheck
	idx := idxBE.OpenUser(info)
	defer idx.Close() //nolint:errcheck

	if err := box.Init(); err != nil {
		return 0, 0, fmt.Errorf("migrate: init %s: %w", user, err)
	}

	entries, err := os.ReadDir(userPath)
	if err != nil {
		return 0, 0, fmt.Errorf("migrate: readdir %s: %w", user, err)
	}

	folders := []struct{ name, path string }{}
	inboxPath := filepath.Join(userPath, "INBOX")
	if _, err := os.Stat(inboxPath); err == nil {
		folders = append(folders, struct{ name, path string }{"INBOX", inboxPath})
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), ".") {
			continue
		}
		name := strings.TrimPrefix(e.Name(), ".")
		folders = append(folders, struct{ name, path string }{name, filepath.Join(userPath, e.Name())})
	}

	for _, f := range folders {
		if f.name != "INBOX" {
			if err := box.Create(f.name); err != nil {
				return migrated, skipped, fmt.Errorf("migrate: create %s/%s: %w", user, f.name, err)
			}
		}
		m, s, err := migrateFolder(box, idx, user, f.name, f.path)
		migrated += m
		skipped += s
		if err != nil {
			return migrated, skipped, err
		}
	}
	return migrated, skipped, nil
}

func migrateFolder(box mailbox.UserMailbox, idx mailbox.UserIndex, user, folder, folderPath string) (migrated, skipped int, _ error) {
	curPath := filepath.Join(folderPath, "cur")
	entries, err := os.ReadDir(curPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("migrate: readdir cur %s/%s: %w", user, folder, err)
	}

	folderMeta, err := idx.OpenFolder(folder, uint32(os.Getpid()))
	if err != nil {
		return 0, 0, fmt.Errorf("migrate: openfolder %s/%s: %w", user, folder, err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		msgPath := filepath.Join(curPath, e.Name())
		flags := maildirFlags(e.Name())

		if *flagDry {
			slog.Info("would migrate", "user", user, "folder", folder, "file", e.Name())
			skipped++
			continue
		}

		info, err := e.Info()
		if err != nil {
			slog.Error("stat failed, skipping", "file", msgPath, "err", err)
			skipped++
			continue
		}

		f, err := os.Open(msgPath)
		if err != nil {
			slog.Error("open failed, skipping", "file", msgPath, "err", err)
			skipped++
			continue
		}

		uid, allocErr := idx.AllocateUID(folderMeta.ID)
		if allocErr != nil {
			f.Close()
			return migrated, skipped, fmt.Errorf("migrate: allocate %s/%s/%s: %w", user, folder, e.Name(), allocErr)
		}
		filename, saveErr := box.Save(folder, f, uid, info.Size(), flags)
		f.Close()
		if saveErr != nil {
			return migrated, skipped, fmt.Errorf("migrate: save %s/%s/%s: %w", user, folder, e.Name(), saveErr)
		}
		if err := idx.AppendMessage(folderMeta.ID, &mailbox.MessageMeta{
			UID:      uid,
			Filename: filename,
			Flags:    flags,
			Size:     uint32(info.Size()),
		}); err != nil {
			return migrated, skipped, fmt.Errorf("migrate: append %s/%s/%s: %w", user, folder, e.Name(), err)
		}
		slog.Info("migrated", "user", user, "folder", folder, "file", e.Name(), "uid", uid)
		migrated++
	}
	return migrated, skipped, nil
}

func userDir(root, user string) string {
	if at := strings.LastIndex(user, "@"); at >= 0 {
		return filepath.Join(root, user[at+1:], user[:at])
	}
	return filepath.Join(root, user)
}

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
