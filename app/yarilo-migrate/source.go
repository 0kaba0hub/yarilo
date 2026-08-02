package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	dboxv1 "github.com/yarilomail/yarilo/internal/storage/mailbox/dbox/v1legacy"
	mdboxv1 "github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox/v1legacy"
)

// sourceMessage is one decoded message handed from a source
// reader to the destination writer. Bodies are materialised in
// memory — acceptable for migration scale (a few-MB per message,
// processed one at a time per goroutine).
type sourceMessage struct {
	Folder       string
	Body         []byte
	Flags        []string
	InternalDate time.Time
	GUID         [16]byte
}

// sourceWalker walks one per-user source tree and yields every
// message via the visitor. Returning a non-nil error from the
// visitor aborts the walk.
type sourceWalker interface {
	Walk(userHome string, visit func(sourceMessage) error) error
}

// pickWalker resolves the --src flag to a walker implementation.
func pickWalker(src string) (sourceWalker, error) {
	switch strings.ToLower(src) {
	case "maildir":
		return maildirWalker{}, nil
	case "dbox-v1", "dboxv1", "yarilo-dbox":
		return dboxV1Walker{}, nil
	case "mdbox-v1", "mdboxv1", "yarilo-mdbox":
		return mdboxV1Walker{}, nil
	default:
		return nil, fmt.Errorf("unknown --src %q (want maildir|dbox-v1|mdbox-v1)", src)
	}
}

// ---- maildir ------------------------------------------------

type maildirWalker struct{}

func (maildirWalker) Walk(home string, visit func(sourceMessage) error) error {
	entries, err := os.ReadDir(home)
	if err != nil {
		return fmt.Errorf("maildir walk: read %s: %w", home, err)
	}
	folders := []struct{ name, path string }{}
	if _, err := os.Stat(filepath.Join(home, "INBOX")); err == nil {
		folders = append(folders, struct{ name, path string }{"INBOX", filepath.Join(home, "INBOX")})
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), ".") {
			continue
		}
		name := strings.TrimPrefix(e.Name(), ".")
		folders = append(folders, struct{ name, path string }{name, filepath.Join(home, e.Name())})
	}
	for _, f := range folders {
		if err := maildirWalkFolder(f.name, f.path, visit); err != nil {
			return err
		}
	}
	return nil
}

func maildirWalkFolder(folder, folderPath string, visit func(sourceMessage) error) error {
	curPath := filepath.Join(folderPath, "cur")
	entries, err := os.ReadDir(curPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("maildir walk: read cur %s: %w", folderPath, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(curPath, e.Name()))
		if err != nil {
			return fmt.Errorf("maildir walk: read %s: %w", e.Name(), err)
		}
		info, _ := e.Info()
		msg := sourceMessage{
			Folder: folder,
			Body:   body,
			Flags:  maildirFlags(e.Name()),
		}
		if info != nil {
			msg.InternalDate = info.ModTime()
		}
		if err := visit(msg); err != nil {
			return err
		}
	}
	return nil
}

// ---- legacy yarilo dbox ------------------------------------

type dboxV1Walker struct{}

func (dboxV1Walker) Walk(home string, visit func(sourceMessage) error) error {
	folders, err := dboxv1.ListFolders(home)
	if err != nil {
		return err
	}
	for _, folder := range folders {
		msgs, err := dboxv1.ListMessages(home, folder)
		if err != nil {
			return err
		}
		for _, name := range msgs {
			m, err := dboxv1.ReadMessage(home, folder, name)
			if err != nil {
				return err
			}
			if err := visit(sourceMessage{
				Folder:       folder,
				Body:         m.Body,
				InternalDate: m.InternalDate,
				GUID:         m.GUID,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- legacy yarilo mdbox -----------------------------------

type mdboxV1Walker struct{}

func (mdboxV1Walker) Walk(home string, visit func(sourceMessage) error) error {
	folders, err := mdboxv1.ListFolders(home)
	if err != nil {
		return err
	}
	for _, folder := range folders {
		entries, err := mdboxv1.ReadMap(home, folder)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.Expunged {
				continue
			}
			m, err := mdboxv1.ReadMessage(home, e)
			if err != nil {
				return err
			}
			if err := visit(sourceMessage{
				Folder:       folder,
				Body:         m.Body,
				InternalDate: m.InternalDate,
				GUID:         m.GUID,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// bodyReader returns an io.Reader for msg.Body so the
// destination Save can stream it without an extra allocation.
func (m sourceMessage) bodyReader() io.Reader { return bytes.NewReader(m.Body) }
