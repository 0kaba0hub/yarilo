// yarilo-migrate converts a Maildir tree to dbox or mdbox format in-place.
// Usage: yarilo-migrate --from /var/mail/vhosts --to /var/mail/dbox --format dbox|mdbox [--dry-run]
package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/dbox"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/mdbox"
)

var (
	flagFrom   = flag.String("from", "", "source Maildir root (required)")
	flagTo     = flag.String("to", "", "destination root (required)")
	flagFormat = flag.String("format", "dbox", "output format: dbox or mdbox")
	flagDry    = flag.Bool("dry-run", false, "print actions without writing")
)

type dst interface {
	Init(user string) error
	Create(user, folder string) error
	Save(user, folder string, r io.Reader, size int64, flags []string) (string, error)
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	flag.Parse()

	if *flagFrom == "" || *flagTo == "" {
		fmt.Fprintln(os.Stderr, "usage: yarilo-migrate --from <src> --to <dst> --format dbox|mdbox")
		os.Exit(1)
	}

	var backend dst
	switch strings.ToLower(*flagFormat) {
	case "dbox":
		d, err := dbox.New(*flagTo)
		if err != nil {
			slog.Error("open dest", "err", err)
			os.Exit(1)
		}
		backend = d
	case "mdbox":
		d, err := mdbox.New(*flagTo)
		if err != nil {
			slog.Error("open dest", "err", err)
			os.Exit(1)
		}
		backend = d
	default:
		slog.Error("unknown format", "format", *flagFormat)
		os.Exit(1)
	}

	var migrated, skipped int
	err := filepath.WalkDir(*flagFrom, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Detect user dirs at depth 2: <root>/<domain>/<localpart>/
		rel, _ := filepath.Rel(*flagFrom, path)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 2 || !d.IsDir() {
			return nil
		}
		user := parts[1] + "@" + parts[0]
		m, s, err := migrateUser(*flagFrom, backend, user)
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

// migrateUser walks all maildir folders for one user.
func migrateUser(srcRoot string, backend dst, user string) (migrated, skipped int, _ error) {
	userPath := userDir(srcRoot, user)

	if err := backend.Init(user); err != nil {
		return 0, 0, fmt.Errorf("migrate: init %s: %w", user, err)
	}

	// Collect folders: INBOX + any .FolderName dirs
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
			if err := backend.Create(user, f.name); err != nil {
				return migrated, skipped, fmt.Errorf("migrate: create %s/%s: %w", user, f.name, err)
			}
		}
		m, s, err := migrateFolder(backend, user, f.name, f.path)
		migrated += m
		skipped += s
		if err != nil {
			return migrated, skipped, err
		}
	}
	return migrated, skipped, nil
}

// migrateFolder copies all messages from a maildir cur/ directory.
func migrateFolder(backend dst, user, folder, folderPath string) (migrated, skipped int, _ error) {
	curPath := filepath.Join(folderPath, "cur")
	entries, err := os.ReadDir(curPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("migrate: readdir cur %s/%s: %w", user, folder, err)
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

		_, saveErr := backend.Save(user, folder, f, info.Size(), flags)
		f.Close()
		if saveErr != nil {
			return migrated, skipped, fmt.Errorf("migrate: save %s/%s/%s: %w", user, folder, e.Name(), saveErr)
		}
		slog.Info("migrated", "user", user, "folder", folder, "file", e.Name())
		migrated++
	}
	return migrated, skipped, nil
}

// userDir returns the maildir root for user@domain → <root>/domain/user.
func userDir(root, user string) string {
	if at := strings.LastIndex(user, "@"); at >= 0 {
		return filepath.Join(root, user[at+1:], user[:at])
	}
	return filepath.Join(root, user)
}

// maildirFlags parses Maildir flag chars from filename ":2,<flags>".
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
